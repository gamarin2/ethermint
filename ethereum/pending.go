package ethereum

import (
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	ethTypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/ethereum/go-ethereum/params"

	abciTypes "github.com/tendermint/abci/types"
	emtTypes "github.com/tendermint/ethermint/types"
)

//----------------------------------------------------------------------
// pending manages concurrent access to the intermediate work object

type pending struct {
	mtx  *sync.Mutex
	work *work
}

func newPending() *pending {
	return &pending{mtx: &sync.Mutex{}}
}

// execute the transaction
func (p *pending) deliverTx(blockchain *core.BlockChain, config *eth.Config, tx *ethTypes.Transaction) error {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	blockHash := common.Hash{}
	return p.work.deliverTx(blockchain, config, blockHash, tx)
}

// accumulate validator rewards
func (p *pending) accumulateRewards(strategy *emtTypes.Strategy) {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	p.work.accumulateRewards(strategy)
}

// commit and reset the work
func (p *pending) commit(blockchain *core.BlockChain, receiver common.Address) (common.Hash, error) {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	blockHash, err := p.work.commit(blockchain)
	if err != nil {
		return common.Hash{}, err
	}

	work, err := p.resetWork(blockchain, receiver)
	if err != nil {
		return common.Hash{}, err
	}

	p.work = work
	return blockHash, err
}

// return a new work object with the latest block and state from the chain
func (p *pending) resetWork(blockchain *core.BlockChain, receiver common.Address) (*work, error) {
	state, err := blockchain.State()
	if err != nil {
		return nil, err
	}

	currentBlock := blockchain.CurrentBlock()
	ethHeader := newBlockHeader(receiver, currentBlock)

	return &work{
		header:       ethHeader,
		parent:       currentBlock,
		state:        state,
		txIndex:      0,
		totalUsedGas: big.NewInt(0),
		gp:           new(core.GasPool).AddGas(ethHeader.GasLimit),
	}, nil
}

func (p *pending) updateHeaderWithTimeInfo(config *params.ChainConfig, parentTime uint64) {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	p.work.updateHeaderWithTimeInfo(config, parentTime)
}

//----------------------------------------------------------------------
// Implements miner.Pending API (our custom patch to go-ethereum)
// TODO: Remove PendingBlock

// Return a new block and a copy of the state from the latest work
func (s *pending) Pending() (*ethTypes.Block, *state.StateDB) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	return ethTypes.NewBlock(
		s.work.header,
		s.work.transactions,
		nil,
		s.work.receipts,
	), s.work.state.Copy()
}

// Return a new block from the latest work
func (s *pending) PendingBlock() *ethTypes.Block {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	return ethTypes.NewBlock(
		s.work.header,
		s.work.transactions,
		nil,
		s.work.receipts,
	)
}

//----------------------------------------------------------------------
//

// The work struct handles block processing.
// It's updated with each DeliverTx and reset on Commit
type work struct {
	header *ethTypes.Header
	parent *ethTypes.Block
	state  *state.StateDB

	txIndex      int
	transactions []*ethTypes.Transaction
	receipts     ethTypes.Receipts
	allLogs      []*ethTypes.Log

	totalUsedGas *big.Int
	gp           *core.GasPool
}

func (w *work) accumulateRewards(strategy *emtTypes.Strategy) {
	core.AccumulateRewards(w.state, w.header, []*ethTypes.Header{})
	w.header.GasUsed = w.totalUsedGas
}

// Runs ApplyTransaction against the ethereum blockchain, fetches any logs,
// and appends the tx, receipt, and logs
func (w *work) deliverTx(blockchain *core.BlockChain, config *eth.Config, blockHash common.Hash, tx *ethTypes.Transaction) error {
	w.state.StartRecord(tx.Hash(), blockHash, w.txIndex)
	receipt, _, err := core.ApplyTransaction(
		config.ChainConfig,
		blockchain,
		w.gp,
		w.state,
		w.header,
		tx,
		w.totalUsedGas,
		vm.Config{EnablePreimageRecording: config.EnablePreimageRecording},
	)
	if err != nil {
		return err
		glog.V(logger.Debug).Infof("DeliverTx error: %v", err)
		return abciTypes.ErrInternalError
	}

	logs := w.state.GetLogs(tx.Hash())

	w.txIndex += 1

	// TODO: allocate correct size in BeginBlock instead of using append
	w.transactions = append(w.transactions, tx)
	w.receipts = append(w.receipts, receipt)
	w.allLogs = append(w.allLogs, logs...)

	return err
}

// Commit the ethereum state, update the header, make a new block and add it
// to the ethereum blockchain. The application root hash is the hash of the ethereum block.
func (w *work) commit(blockchain *core.BlockChain) (common.Hash, error) {
	// commit ethereum state and update the header
	hashArray, err := w.state.Commit(false) // XXX: ugh hardforks
	if err != nil {
		return common.Hash{}, err
	}
	w.header.Root = hashArray

	// tag logs with state root
	// NOTE: BlockHash ?
	for _, log := range w.allLogs {
		log.BlockHash = hashArray
	}

	// create block object and compute final commit hash (hash of the ethereum block)
	block := ethTypes.NewBlock(w.header, w.transactions, nil, w.receipts)
	blockHash := block.Hash()

	// save the block to disk
	glog.V(logger.Debug).Infof("Committing block with state hash %X and root hash %X", hashArray, blockHash)
	_, err = blockchain.InsertChain([]*ethTypes.Block{block})
	if err != nil {
		glog.V(logger.Debug).Infof("Error inserting ethereum block in chain: %v", err)
		return common.Hash{}, err
	}
	return blockHash, err
}

func (w *work) updateHeaderWithTimeInfo(config *params.ChainConfig, parentTime uint64) {
	lastBlock := w.parent
	w.header.Time = new(big.Int).SetUint64(parentTime)
	w.header.Difficulty = core.CalcDifficulty(config, parentTime,
		lastBlock.Time().Uint64(), lastBlock.Number(), lastBlock.Difficulty())
}

//----------------------------------------------------------------------

// Create a new block header from the previous block
func newBlockHeader(receiver common.Address, prevBlock *ethTypes.Block) *ethTypes.Header {
	return &ethTypes.Header{
		Number:     prevBlock.Number().Add(prevBlock.Number(), big.NewInt(1)),
		ParentHash: prevBlock.Hash(),
		GasLimit:   core.CalcGasLimit(prevBlock),
		Coinbase:   receiver,
	}
}