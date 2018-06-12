package main

import (
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	influxClient "github.com/influxdata/influxdb/client/v2"
	"log"
	"strconv"
	"sync"
	//"time"
)

/*

This program sets up a RPC connection with a local bitcoind instance,
and an HTTP client for a local influxdb instance.

Goal 1:
  Analyze 2k blocks for some basic statistics in influxdb
  Analyze performance of software (time spent per block, which parts are most parallelizable, etc.)
  Hook up to Grafana and show some of the basic plots.

TODO:
  Decide how to best figure out fee breakdowns
  Is it enough to get quartiles, or deciles?

  Use "Another coin bites the dust" metrics for determining number of dust outputs created?

*/

const BLOCK_NUM_DIFF = 6

// Consts for influxdb
// TODO: Put some of these in environment variables
const (
	DB          = "btctest"
	DB_USERNAME = "marcin"
	DB_PASSWORD = "af181a9c33573928734a387223384b9a318ebb36"

	BITCOIND_HOST     = "localhost:8332"
	BITCOIND_USERNAME = "marcin"
	BITCOIND_PASSWORD = "af337a17c853e43e6393153e8d868578789ca20a"
)

type Dashboard struct {
	client  *rpcclient.Client
	iClient influxClient.Client
	bp      influxClient.BatchPoints
}

func setupDashboard() Dashboard {
	// Connect to local bitcoin core RPC server using HTTP POST mode.
	connCfg := &rpcclient.ConnConfig{
		Host:         BITCOIND_HOST,
		User:         BITCOIND_USERNAME,
		Pass:         BITCOIND_PASSWORD,
		HTTPPostMode: true, // Bitcoin core only supports HTTP POST mode
		DisableTLS:   true, // Bitcoin core does not provide TLS by default
	}
	// Notice the notification parameter is nil since notifications are
	// not supported in HTTP POST mode.
	client, err := rpcclient.New(connCfg, nil)
	if err != nil {
		log.Fatal(err)
	}

	// Setup influxdb client.
	ic, err := influxClient.NewHTTPClient(influxClient.HTTPConfig{
		Addr:     "http://localhost:8086",
		Username: DB_USERNAME,
		Password: DB_PASSWORD,
	})
	if err != nil {
		log.Fatal(err)
	}

	// Setup influx batchpoints.
	bp, err := influxClient.NewBatchPoints(influxClient.BatchPointsConfig{
		Database: "btctest",
	})
	if err != nil {
		log.Fatal(err)
	}

	dash := Dashboard{
		client,
		ic,
		bp,
	}

	return dash
}

func (dash *Dashboard) shutdown() {
	dash.client.Shutdown()
	dash.iClient.Close()
}

func main() {
	dash := setupDashboard()
	defer dash.shutdown()

	// TODO: write tests that check analysis functions.
	dash.outputDetectionTest()

	dash.analysisTest()

	/*
		        // This update loop should be used once the entire chain history has been processed.
			// Update DB when new blocks are detected.

		analyzedCount := int64(0) // TODO: read from db
			for {
				// Get the current block count.
				blockCount, err := client.GetBlockCount()
				if err != nil {
					log.Fatal(err)
				}
				log.Printf("Block count: %d", blockCount)

				// Only do things if the block is 6 confirmations in.
				if blockCount-analyzedCount > BLOCK_NUM_DIFF {
					dash.analyzeBlock(analyzedCount)
				} else {
					time.Sleep(100 * time.Millisecond)
				}

				return
			}
	*/
}

func (dash *Dashboard) analysisTest() {
	// TODO
	// Setup thread to receive new batchpoints from workers. and put into db

	const START_BLOCK = 525001
	const END_BLOCK = 525025

	for i := START_BLOCK; i < END_BLOCK; i++ {
		dash.analyzeBlock(int64(i))
	}

	log.Println(dash.bp)

	err := dash.iClient.Write(dash.bp)
	if err != nil {
		log.Println("DB WRITE ERR: ", err)
	}
}

// Fields (don't need to be indexed)
type BlockStatistics struct {
	numTxns                       int
	numTxnsSpendingP2SH           int
	numTxnsSpendingP2WPKH         int
	numTxnsSpendingP2WSH          int
	numTxnsSendingToNativeWitness int
	numTxnsSignalingRBF           int
	numTxnsThatBatch              int
	numTxnsThatConsolidate        int
}

func (dash *Dashboard) analyzeBlock(blockHeight int64) {
	// Get hash of this block.
	blockHash, err := dash.client.GetBlockHash(blockHeight)
	if err != nil {
		log.Fatal("Error getting block hash", err)
	}

	// Get contents of this block.
	block, err := dash.client.GetBlock(blockHash)
	if err != nil {
		log.Fatal("error getting block")
	}

	blockTime := block.Header.Timestamp
	numTxns := float64(len(block.Transactions))

	// Fields stored in a struct (don't need to be indexed)
	blockStats := BlockStatistics{numTxns: len(block.Transactions)}

	tags := make(map[string]string)
	fields := make(map[string]interface{})

	// Tags (get indexed by influxdb)
	// (timestamp implicitly indexed because it's a time-series db)
	// TODO: decide if block timestamp is the way to go
	// blockheight,

	var wg sync.WaitGroup
	resultsCh := make(chan BlockStatistics, len(block.Transactions))
	for _, txn := range block.Transactions {
		wg.Add(1)

		go func(txn *wire.MsgTx) {
			statsDiff := analyzeTxn(txn)
			resultsCh <- statsDiff
			wg.Done()
		}(txn)
	}
	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	// Combine results as they come in from each transactions thread.
	for res := range resultsCh {
		blockStats.numTxnsSpendingP2SH += res.numTxnsSpendingP2SH
		blockStats.numTxnsSpendingP2WPKH += res.numTxnsSpendingP2WPKH
		blockStats.numTxnsSpendingP2WSH += res.numTxnsSpendingP2WSH
		blockStats.numTxnsSendingToNativeWitness += res.numTxnsSendingToNativeWitness
		blockStats.numTxnsSignalingRBF += res.numTxnsSignalingRBF
		blockStats.numTxnsThatBatch += res.numTxnsThatBatch
		blockStats.numTxnsThatConsolidate += res.numTxnsThatConsolidate
	}

	tags["height"] = strconv.Itoa(int(blockHeight))
	fields["spend_P2SH"] = float64(blockStats.numTxnsSpendingP2SH) / numTxns
	fields["spend_P2WPKH"] = float64(blockStats.numTxnsSpendingP2WPKH) / numTxns
	fields["spend_P2WSH"] = float64(blockStats.numTxnsSpendingP2WSH) / numTxns
	fields["sent_to_native_witness"] = float64(blockStats.numTxnsSendingToNativeWitness) / numTxns
	fields["num_signalling_RBF"] = float64(blockStats.numTxnsSignalingRBF) / numTxns
	fields["num_batching"] = float64(blockStats.numTxnsThatBatch) / numTxns
	fields["num_consolidating"] = float64(blockStats.numTxnsThatConsolidate) / numTxns

	pt, err := influxClient.NewPoint(
		"block_stats",
		tags,
		fields,
		blockTime,
	)

	log.Println("Block and statistics:", blockHeight, blockStats)

	dash.bp.AddPoint(pt)
}

// TODO: add native witness output detection.
func analyzeTxn(txn *wire.MsgTx) BlockStatistics {
	const RBF_THRESHOLD = uint32(0xffffffff) - 1
	const CONSOLIDATION_MIN = 3 // Minimum number of inputs for it to be considered consolidation.
	const BATCHING_MIN = 3      // Minimum number of outputs for it to be considered batching.

	statsDiff := BlockStatistics{}

	for _, input := range txn.TxIn {
		//  A transaction signals RBF any of if its input's sequence number is less than (0xffffffff - 1).
		if input.Sequence < RBF_THRESHOLD {
			statsDiff.numTxnsSignalingRBF = 1
			break
		}
	}

	for _, output := range txn.TxOut {
		if outputIsP2SH(output) {
			statsDiff.numTxnsSpendingP2SH = 1
		}

		if outputIsP2WSH(output) {
			statsDiff.numTxnsSpendingP2WSH = 1
		}

		if outputIsP2WPKH(output) {
			statsDiff.numTxnsSpendingP2WPKH = 1
		}
	}

	if (len(txn.TxIn) >= CONSOLIDATION_MIN) && (len(txn.TxOut) == 1) {
		statsDiff.numTxnsThatConsolidate = 1
	}

	if len(txn.TxOut) >= BATCHING_MIN {
		statsDiff.numTxnsThatBatch = 1
	}

	return statsDiff
}

func outputIsP2SH(txOut *wire.TxOut) bool {
	const OP_HASH160 = 0xa9
	const OP_EQUAL = 0x87

	scriptPubKey := txOut.PkScript

	// Check the length.
	if len(scriptPubKey) != 23 {
		return false
	}

	if (scriptPubKey[0] != OP_HASH160) || (scriptPubKey[22] != OP_EQUAL) {
		return false
	}

	return true
}

func outputIsP2WPKH(txOut *wire.TxOut) bool {
	scriptPubKey := txOut.PkScript

	// Check the version byte and the length of the witness program.
	if (scriptPubKey[0] == 0) && (scriptPubKey[1] == 20) && len(scriptPubKey) == 22 {
		return true
	} else {
		return false
	}
}

func outputIsP2WSH(txOut *wire.TxOut) bool {
	scriptPubKey := txOut.PkScript

	// Check the version byte and the length of the witness program.
	if (scriptPubKey[0] == 0) && (scriptPubKey[1] == 32) && len(scriptPubKey) == 34 {
		return true
	} else {
		return false
	}
}

func (dash *Dashboard) outputDetectionTest() {
	// TODO: find P2SH, P2WPKH, P2WSH, nativfe iwtness otpt
	// transactions to test above functions on.
}
