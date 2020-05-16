package common

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zcash/lightwalletd/parser"
)

type darksideState struct {
	resetted          bool
	startHeight       int // activeBlocks[0] corresponds to this height
	saplingActivation int // must be <= startHeight
	branchID          string
	chainName         string
	cache             *BlockCache
	mutex             sync.RWMutex

	// This is the highest (latest) block height currently being presented
	// by the mock zcashd.
	latestHeight int

	// These blocks (up to and including tip) are presented by mock zcashd.
	// activeBlocks[0] is the block at height startHeight.
	activeBlocks [][]byte // full blocks, binary, as from zcashd getblock rpc

	// Staged blocks are waiting to be applied (by ApplyStaged()) to activeBlocks.
	// They are in order of arrival (not necessarily sorted by height), and are
	// applied in arrival order.
	stagedBlocks [][]byte // full blocks, binary

	// These are full transactions as received from the wallet by SendTransaction().
	// They are conceptually in the mempool. They are not yet available to be fetched
	// by GetTransaction(). They can be fetched by darkside GetIncomingTransaction().
	incomingTransactions [][]byte

	// These transactions come from StageTransactions(); they will be merged into
	// activeBlocks by ApplyStaged() (and this list then cleared).
	stagedTransactions []stagedTx
}

var state darksideState

// DarksideIsEnabled returns true if we're in darkside (test) mode.
func DarksideIsEnabled() bool {
	return state.resetted
}

type stagedTx struct {
	height int
	bytes  []byte
}

// DarksideInit should be called once at startup in darksidewalletd mode.
func DarksideInit(c *BlockCache) {
	state.cache = c
	RawRequest = darksideRawRequest
	go func() {
		time.Sleep(30 * time.Minute)
		Log.Fatal("Shutting down darksidewalletd to prevent accidental deployment in production.")
	}()
}

// DarksideSetMetaState allows the wallet test code to specify values
// that are returned by GetLightdInfo().
func DarksideReset(sa int, bi, cn string) error {
	stopIngestor()
	state = darksideState{
		resetted:             true,
		saplingActivation:    sa,
		latestHeight:         -1,
		branchID:             bi,
		chainName:            cn,
		cache:                state.cache,
		mutex:                state.mutex,
		activeBlocks:         make([][]byte, 0),
		stagedBlocks:         make([][]byte, 0),
		incomingTransactions: make([][]byte, 0),
		stagedTransactions:   make([]stagedTx, 0),
	}
	state.cache.Reset(sa)
	return nil
}

// DarksideAddBlock adds a single block to the active blocks list.
func addBlockActive(blockBytes []byte) error {
	block := parser.NewBlock()
	rest, err := block.ParseFromSlice(blockBytes)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return errors.New("block serialization is too long")
	}
	blockHeight := block.GetHeight()
	// first block, add to existing blocks slice if possible
	if blockHeight > state.startHeight+len(state.activeBlocks) {
		// The new block can't contiguously extend the existing
		// range, so we have to drop the existing range.
		state.activeBlocks = state.activeBlocks[:0]
	} else if blockHeight < state.startHeight {
		// This block will replace the entire existing range.
		state.activeBlocks = state.activeBlocks[:0]
	} else {
		// Drop the block that will be overwritten, and its children.
		state.activeBlocks = state.activeBlocks[:blockHeight-state.startHeight]
	}
	if len(state.activeBlocks) == 0 {
		state.startHeight = blockHeight
	} else {
		// Set this block's prevhash.
		prevblock := parser.NewBlock()
		rest, err := prevblock.ParseFromSlice(state.activeBlocks[len(state.activeBlocks)-1])
		if err != nil {
			return err
		}
		if len(rest) != 0 {
			return errors.New("block is too long")
		}
		copy(blockBytes[4:4+32], prevblock.GetEncodableHash())
	}
	state.activeBlocks = append(state.activeBlocks, blockBytes)
	return nil
}

// Set the prev hashes of the blocks in the active chain
func setPrevhash() {
	prevhash := make([]byte, 32)
	for _, blockBytes := range state.activeBlocks {
		// Set this block's prevhash.
		block := parser.NewBlock()
		rest, err := block.ParseFromSlice(blockBytes)
		if err != nil {
			Log.Fatal(err)
		}
		if len(rest) != 0 {
			Log.Fatal(errors.New("block is too long"))
		}
		copy(blockBytes[4:4+32], prevhash)
		prevhash = block.GetEncodableHash()
	}
}

// DarksideApplyStaged moves the staging area to the active block list.
// If this returns an error, the state could be weird; perhaps it may
// be better to simply crash.
func DarksideApplyStaged(height int) error {
	state.mutex.Lock()
	defer state.mutex.Unlock()
	if !state.resetted {
		return errors.New("please call Reset first")
	}
	// Move the staged blocks into active list
	for _, blockBytes := range state.stagedBlocks {
		if err := addBlockActive(blockBytes); err != nil {
			return err
		}
	}
	state.stagedBlocks = state.stagedBlocks[:0]

	// Add staged transactions into blocks. Note we're not trying to
	// recover to the initial state; maybe it's better to just crash
	// on errors.
	for _, tx := range state.stagedTransactions {
		if tx.height < state.startHeight {
			return errors.New("transaction height too low")
		}
		if tx.height >= state.startHeight+len(state.activeBlocks) {
			return errors.New("transaction height too high")
		}
		block := state.activeBlocks[tx.height-state.startHeight]
		if block[1487] == 253 {
			return errors.New("too many transactions in a block (max 253)")
		}
		block[1487]++ // one more transaction
		block[68]++   // hack HashFinalSaplingRoot to mod the block hash
		block = append(block, tx.bytes...)
		state.activeBlocks[tx.height-state.startHeight] = block
	}
	if len(state.stagedTransactions) > 0 {
		return errors.New("one or more transactions have no block")
	}
	state.stagedTransactions = state.stagedTransactions[:0]
	setPrevhash()
	state.latestHeight = height

	// The block ingestor can only run if there are blocks
	if len(state.activeBlocks) > 0 {
		startIngestor(state.cache)
	} else {
		stopIngestor()
	}
	return nil
}

// DarksideGetIncomingTransactions returns all transactions we're
// received via SendTransaction().
func DarksideGetIncomingTransactions() [][]byte {
	return state.incomingTransactions
}

// DarksideStageBlocks opens and reads blocks from the given URL and
// adds them to the staging area.
func DarksideStageBlocks(url string) error {
	if !state.resetted {
		return errors.New("please call Reset first")
	}
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// some blocks are too large, especially when encoded in hex, for the
	// default buffer size, so set up a larger one; 8mb should be enough.
	scan := bufio.NewScanner(resp.Body)
	var scanbuf []byte
	scan.Buffer(scanbuf, 8*1000*1000)
	for scan.Scan() { // each line (block)
		blockHex := scan.Text()
		if blockHex == "404: Not Found" {
			// special case error (http resource not found, bad pathname)
			return errors.New(blockHex)
		}
		blockBytes, err := hex.DecodeString(blockHex)
		if err != nil {
			return err
		}
		state.stagedBlocks = append(state.stagedBlocks, blockBytes)
	}
	return scan.Err()
}

// DarksideStageBlockStream adds the block to the staging area
func DarksideStageBlockStream(blockHex string) error {
	blockBytes, err := hex.DecodeString(blockHex)
	if err != nil {
		return err
	}
	state.stagedBlocks = append(state.stagedBlocks, blockBytes)
	return nil
}

// DarksideStageBlocksCreate creates empty blocks and adds them to the staging area.
func DarksideStageBlocksCreate(height int32, nonce int32, count int32) error {
	if !state.resetted {
		return errors.New("please call Reset first")
	}
	for i := 0; i < int(count); i++ {

		fakeCoinbase := "0400008085202f890100000000000000000000000000000000000000000000000000" +
			"00000000000000ffffffff2a03d12c0c00043855975e464b8896790758f824ceac97836" +
			"22c17ed38f1669b8a45ce1da857dbbe7950e2ffffffff02a0ebce1d000000001976a914" +
			"7ed15946ec14ae0cd8fa8991eb6084452eb3f77c88ac405973070000000017a914e445cf" +
			"a944b6f2bdacefbda904a81d5fdd26d77f8700000000000000000000000000000000000000"

		// This coinbase transaction was pulled from block 797905, whose
		// little-endian encoding is 0xD12C0C00. Replace it with the block
		// number we want.
		fakeCoinbase = strings.Replace(fakeCoinbase, "d12c0c00",
			fmt.Sprintf("%02x", height&0xFF)+
				fmt.Sprintf("%02x", (height>>8)&0xFF)+
				fmt.Sprintf("%02x", (height>>16)&0xFF)+
				fmt.Sprintf("%02x", (height>>24)&0xFF), 1)
		fakeCoinbaseBytes, err := hex.DecodeString(fakeCoinbase)
		if err != nil {
			Log.Fatal(err)
		}

		hashOfTxnsAndHeight := sha256.Sum256([]byte(string(nonce) + "#" + string(height)))
		blockHeader := &parser.BlockHeader{
			RawBlockHeader: &parser.RawBlockHeader{
				Version:              4,                      // start: 0
				HashPrevBlock:        make([]byte, 32),       // start: 4
				HashMerkleRoot:       hashOfTxnsAndHeight[:], // start: 36
				HashFinalSaplingRoot: make([]byte, 32),       // start: 68
				Time:                 1,                      // start: 100
				NBitsBytes:           make([]byte, 4),        // start: 104
				Nonce:                make([]byte, 32),       // start: 108
				Solution:             make([]byte, 1344),     // starts: 140, 143
			}, // length: 1487
		}

		headerBytes, err := blockHeader.MarshalBinary()
		if err != nil {
			Log.Fatal(err)
		}
		blockBytes := make([]byte, 0)
		blockBytes = append(blockBytes, headerBytes...)
		blockBytes = append(blockBytes, byte(1))
		blockBytes = append(blockBytes, fakeCoinbaseBytes...)
		state.stagedBlocks = append(state.stagedBlocks, blockBytes)
		height++
	}
	return nil
}

// DarksideClearIncomingTransactions empties the incoming transaction list.
func DarksideClearIncomingTransactions() {
	state.incomingTransactions = make([][]byte, 0)
}

// DarksideSendTransaction is the handler for the SendTransaction gRPC.
// Save the transaction in the incoming transactions list.
func DarksideSendTransaction(txHex []byte) ([]byte, error) {
	if !state.resetted {
		return nil, errors.New("please call Reset first")
	}
	// Need to parse the transaction to return its hash, plus it's
	// good error checking.
	txBytes, err := hex.DecodeString(string(txHex))
	if err != nil {
		return nil, err
	}
	tx := parser.NewTransaction()
	rest, err := tx.ParseFromSlice(txBytes)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, errors.New("transaction serialization is too long")
	}
	state.incomingTransactions = append(state.incomingTransactions, txBytes)
	return tx.GetDisplayHash(), nil
}

func darksideRawRequest(method string, params []json.RawMessage) (json.RawMessage, error) {
	switch method {
	case "getblockchaininfo":
		blockchaininfo := Blockchaininfo{
			Chain: state.chainName,
			Upgrades: map[string]Upgradeinfo{
				"76b809bb": {ActivationHeight: state.saplingActivation},
			},
			Headers:   state.latestHeight,
			Consensus: ConsensusInfo{state.branchID, state.branchID},
		}
		return json.Marshal(blockchaininfo)

	case "getblock":
		var heightStr string
		err := json.Unmarshal(params[0], &heightStr)
		if err != nil {
			return nil, errors.New("failed to parse getblock request")
		}

		height, err := strconv.Atoi(heightStr)
		if err != nil {
			return nil, errors.New("error parsing height as integer")
		}
		state.mutex.RLock()
		defer state.mutex.RUnlock()
		const notFoundErr = "-8:"
		if len(state.activeBlocks) == 0 {
			return nil, errors.New(notFoundErr)
		}
		if height > state.latestHeight {
			return nil, errors.New(notFoundErr)
		}
		index := height - state.startHeight
		return []byte("\"" + hex.EncodeToString(state.activeBlocks[index]) + "\""), nil

	case "getaddresstxids":
		// Not required for minimal reorg testing.
		return nil, errors.New("not implemented yet")

	case "getrawtransaction":
		return darksideGetRawTransaction(params)

	case "sendrawtransaction":
		var rawtx string
		err := json.Unmarshal(params[0], &rawtx)
		if err != nil {
			return nil, errors.New("failed to parse sendrawtransaction JSON")
		}
		txbytes, err := hex.DecodeString(rawtx)
		if err != nil {
			return nil, errors.New("failed to parse sendrawtransaction value as a hex string")
		}
		state.incomingTransactions = append(state.incomingTransactions, txbytes)
		return nil, nil

	default:
		return nil, errors.New("there was an attempt to call an unsupported RPC")
	}
}

func darksideGetRawTransaction(params []json.RawMessage) (json.RawMessage, error) {
	if !state.resetted {
		return nil, errors.New("please call Reset first")
	}
	// remove the double-quotes from the beginning and end of the hex txid string
	txbytes, err := hex.DecodeString(string(params[0][1 : 1+64]))
	if err != nil {
		return nil, errors.New("-9: " + err.Error())
	}
	// Linear search for the tx, somewhat inefficient but this is test code
	// and there aren't many blocks. If this becomes a performance problem,
	// we can maintain a map of transactions indexed by txid.
	for _, b := range state.activeBlocks {
		block := parser.NewBlock()
		rest, err := block.ParseFromSlice(b)
		if err != nil {
			// this would be strange; we've already parsed this block
			return nil, errors.New("-9: " + err.Error())
		}
		if len(rest) != 0 {
			return nil, errors.New("-9: block serialization is too long")
		}
		for _, tx := range block.Transactions() {
			if bytes.Equal(tx.GetDisplayHash(), txbytes) {
				reply := struct {
					Hex    string `json:"hex"`
					Height int    `json:"height"`
				}{hex.EncodeToString(tx.Bytes()), block.GetHeight()}
				return json.Marshal(reply)
			}
		}
	}
	return nil, errors.New("-5: No information available about transaction")
}

// DarksideStageTransaction adds the given transaction to the staging area.
func DarksideStageTransaction(height int, txBytes []byte) error {
	if !state.resetted {
		return errors.New("please call Reset first")
	}
	state.stagedTransactions = append(state.stagedTransactions,
		stagedTx{
			height: height,
			bytes:  txBytes,
		})
	return nil
}
