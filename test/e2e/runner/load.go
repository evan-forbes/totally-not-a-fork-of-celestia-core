package main

import (
	"container/ring"
	"context"
	"fmt"
	"math/rand"
	"time"

	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	e2e "github.com/tendermint/tendermint/test/e2e/pkg"
	"github.com/tendermint/tendermint/types"
)

// Load generates transactions against the network until the given context is
// canceled.
func Load(ctx context.Context, testnet *e2e.Testnet) error {
	// Since transactions are executed across all nodes in the network, we need
	// to reduce transaction load for larger networks to avoid using too much
	// CPU. This gives high-throughput small networks and low-throughput large ones.
	// This also limits the number of TCP connections, since each worker has
	// a connection to all nodes.
	concurrency := len(testnet.Nodes) * 8
	if concurrency > 64 {
		concurrency = 64
	}

	chTx := make(chan types.Tx)
	chSuccess := make(chan int) // success counts per iteration
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Spawn job generator and processors.
	logger.Info("starting transaction load",
		"workers", concurrency,
		"nodes", len(testnet.Nodes),
		"tx", testnet.TxSize)

	started := time.Now()

	go loadGenerate(ctx, chTx, testnet.TxSize)

	for w := 0; w < concurrency; w++ {
		go loadProcess(ctx, testnet, chTx, chSuccess)
	}

	// Montior transaction to ensure load propagates to the network
	//
	// This loop doesn't check or time out for stalls, since a stall here just
	// aborts the load generator sooner and could obscure backpressure
	// from the test harness, and there are other checks for
	// stalls in the framework. Ideally we should monitor latency as a guide
	// for when to give up, but we don't have a good way to track that yet.
	success := 0
	for {
		select {
		case numSeen := <-chSuccess:
			success += numSeen
		case <-ctx.Done():
			if success == 0 {
				return fmt.Errorf("failed to submit transactions in %s by %d workers",
					time.Since(started), concurrency)
			}

			// TODO perhaps allow test networks to
			// declare required transaction rates, which
			// might allow us to avoid the special case
			// around 0 txs above.
			rate := float64(success) / time.Since(started).Seconds()

			logger.Info("ending transaction load",
				"dur_secs", time.Since(started).Seconds(),
				"txns", success,
				"workers", concurrency,
				"rate", rate)

			return nil
		}
	}
}

// loadGenerate generates jobs until the context is canceled.
//
// The chTx has multiple consumers, thus the rate limiting of the load
// generation is primarily the result of backpressure from the
// broadcast transaction, though there is still some timer-based
// limiting.
func loadGenerate(ctx context.Context, chTx chan<- types.Tx, size int64) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	defer close(chTx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		// We keep generating the same 100 keys over and over, with different values.
		// This gives a reasonable load without putting too much data in the app.
		id := rand.Int63() % 100 // nolint: gosec

		bz := make([]byte, size)
		_, err := rand.Read(bz) // nolint: gosec
		if err != nil {
			panic(fmt.Sprintf("Failed to read random bytes: %v", err))
		}
		tx := types.Tx(fmt.Sprintf("load-%X=%x", id, bz))

		select {
		case <-ctx.Done():
			return
		case chTx <- tx:
			// sleep for a bit before sending the
			// next transaction.
			timer.Reset(loadGenerateWaitTime(size))
		}

	}
}

func loadGenerateWaitTime(size int64) time.Duration {
	const (
		min = int64(10 * time.Millisecond)
		max = int64(100 * time.Millisecond)
	)

	var (
		baseJitter = rand.Int63n(max-min+1) + min // nolint: gosec
		sizeFactor = size * int64(time.Millisecond)
		sizeJitter = rand.Int63n(sizeFactor-min+1) + min // nolint: gosec
		waitTime   = time.Duration(baseJitter + sizeJitter)
	)

	if size == 1 {
		return waitTime / 2
	}

	return waitTime
}

// loadProcess processes transactions
func loadProcess(ctx context.Context, testnet *e2e.Testnet, chTx <-chan types.Tx, chSuccess chan<- int) {
	// Each worker gets its own client to each usable node, which
	// allows for some concurrency while still bounding it.
	clients := make([]*rpchttp.HTTP, 0, len(testnet.Nodes))

	for idx := range testnet.Nodes {
		// Construct a list of usable nodes for the creating
		// load. Don't send load through seed nodes because
		// they do not provide the RPC endpoints required to
		// broadcast transaction.
		if testnet.Nodes[idx].Mode == e2e.ModeSeed {
			continue
		}

		client, err := testnet.Nodes[idx].Client()
		if err != nil {
			continue
		}

		clients = append(clients, client)
	}

	if len(clients) == 0 {
		panic("no clients to process load")
	}

	// Put the clients in a ring so they can be used in a
	// round-robin fashion.
	clientRing := ring.New(len(clients))
	for idx := range clients {
		clientRing.Value = clients[idx]
		clientRing = clientRing.Next()
	}

	successes := 0
	for {
		select {
		case <-ctx.Done():
			return
		case tx := <-chTx:
			clientRing = clientRing.Next()
			client := clientRing.Value.(*rpchttp.HTTP)

			if status, err := client.Status(ctx); err != nil {
				continue
			} else if status.SyncInfo.CatchingUp {
				continue
			}

			if _, err := client.BroadcastTxSync(ctx, tx); err != nil {
				continue
			}
			successes++

			select {
			case chSuccess <- successes:
				successes = 0 // reset counter for the next iteration
				continue
			case <-ctx.Done():
				return
			default:
			}

		}
	}
}
