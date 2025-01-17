package protocol

import (
	"fmt"
	"github.com/deckarep/golang-set"
	"github.com/idena-network/idena-go/blockchain"
	"github.com/idena-network/idena-go/blockchain/types"
	"github.com/idena-network/idena-go/common"
	"github.com/idena-network/idena-go/common/eventbus"
	"github.com/idena-network/idena-go/config"
	"github.com/idena-network/idena-go/core/appstate"
	"github.com/idena-network/idena-go/core/state"
	"github.com/idena-network/idena-go/core/state/snapshot"
	"github.com/idena-network/idena-go/core/upgrade"
	"github.com/idena-network/idena-go/core/validators"
	"github.com/idena-network/idena-go/events"
	"github.com/idena-network/idena-go/ipfs"
	"github.com/idena-network/idena-go/keystore"
	"github.com/idena-network/idena-go/log"
	"github.com/idena-network/idena-go/subscriptions"
	"github.com/pkg/errors"
	"os"
	"time"
)

const FastSyncBatchSize = 1000

type fastSync struct {
	pm                   *IdenaGossipHandler
	log                  log.Logger
	chain                *blockchain.Blockchain
	batches              chan *batch
	ipfs                 ipfs.Proxy
	isSyncing            bool
	appState             *appstate.AppState
	potentialForkedPeers mapset.Set
	manifest             *snapshot.Manifest
	identityStateDB      *state.IdentityStateDB
	validators           *validators.ValidatorsCache
	sm                   *state.SnapshotManager
	bus                  eventbus.Bus
	deferredHeaders      []blockPeer
	coinBase             common.Address
	keyStore             *keystore.KeyStore
	subManager           *subscriptions.Manager
	upgrader             *upgrade.Upgrader
	prevConfig           *config.ConsensusConf

	pubKeyToAddrCache map[string]common.Address
}

func (fs *fastSync) batchSize() uint64 {
	return FastSyncBatchSize
}

func NewFastSync(pm *IdenaGossipHandler, log log.Logger,
	chain *blockchain.Blockchain,
	ipfs ipfs.Proxy,
	appState *appstate.AppState,
	potentialForkedPeers mapset.Set,
	manifest *snapshot.Manifest, sm *state.SnapshotManager, bus eventbus.Bus, coinbase common.Address, keyStore *keystore.KeyStore,
	subManager *subscriptions.Manager, upgrader *upgrade.Upgrader) *fastSync {

	return &fastSync{
		appState:             appState,
		log:                  log,
		potentialForkedPeers: potentialForkedPeers,
		chain:                chain,
		batches:              make(chan *batch, 10),
		pm:                   pm,
		isSyncing:            true,
		ipfs:                 ipfs,
		manifest:             manifest,
		sm:                   sm,
		bus:                  bus,
		coinBase:             coinbase,
		keyStore:             keyStore,
		subManager:           subManager,
		upgrader:             upgrader,
		pubKeyToAddrCache:    map[string]common.Address{},
	}
}

func (fs *fastSync) createPreliminaryCopy(height uint64) (*state.IdentityStateDB, error) {
	return fs.appState.IdentityState.CreatePreliminaryCopy(height)
}

func (fs *fastSync) dropPreliminaries() {
	fs.chain.RemovePreliminaryHead(nil)
	fs.chain.RemovePreliminaryConsensusVersion()
	fs.chain.RemovePreliminaryIntermediateGenesis()
	if fs.prevConfig != nil {
		fs.upgrader.RevertConfig(fs.prevConfig)
		fs.prevConfig = nil
	}
	fs.appState.IdentityState.DropPreliminary()
	fs.identityStateDB = nil
}

func (fs *fastSync) loadValidators() {
	fs.validators = validators.NewValidatorsCache(fs.identityStateDB, fs.appState.State.GodAddress())
	fs.validators.Load()
}

func (fs *fastSync) tryUpgradeConsensus(header *types.Header) {
	if header.ProposedHeader != nil && header.ProposedHeader.Upgrade == uint32(fs.upgrader.Target()) {
		fs.log.Info("Detect upgrade block while fast syncing", "upgrade", fs.upgrader.Target())
		fs.prevConfig = fs.upgrader.UpgradeConfigTo(header.ProposedHeader.Upgrade)
		fs.chain.WritePreliminaryConsensusVersion(header.ProposedHeader.Upgrade)
		fs.upgrader.MigrateIdentityStateDb()
	}
}

func (fs *fastSync) preConsuming(head *types.Header) (from uint64, err error) {
	if fs.chain.PreliminaryHead == nil {
		fs.chain.PreliminaryHead = head
		fs.identityStateDB, err = fs.createPreliminaryCopy(head.Height())
		if err != nil {
			return 0, err
		}
		from = head.Height() + 1
		fs.loadValidators()
		return from, err
	}
	if ver := fs.chain.ReadPreliminaryConsensusVersion(); ver > 0 {
		fs.prevConfig = fs.upgrader.UpgradeConfigTo(ver)
	}
	fs.tryUpgradeConsensus(fs.chain.PreliminaryHead)
	fs.identityStateDB, err = fs.appState.IdentityState.LoadPreliminary(fs.chain.PreliminaryHead.Height())
	if err != nil {
		fs.dropPreliminaries()
		return fs.preConsuming(head)
	}

	fs.loadValidators()
	from = fs.chain.PreliminaryHead.Height() + 1
	return from, nil
}

func (fs *fastSync) applyDeferredBlocks() (uint64, error) {
	defer func() {
		fs.deferredHeaders = []blockPeer{}
	}()

	for _, b := range fs.deferredHeaders {

		if err := fs.validateIdentityState(b); err != nil {
			fs.pm.BanPeer(b.peerId, err)
			return b.Header.Height(), err
		}
		if !b.IdentityDiff.Empty() {
			fs.identityStateDB.CommitTree(int64(b.Header.Height()))
		}

		if err := fs.chain.AddHeaderUnsafe(b.Header); err != nil {
			fs.pm.BanPeer(b.peerId, err)
			return b.Header.Height(), err
		}
		fs.tryUpgradeConsensus(b.Header)

		if b.Header.Flags().HasFlag(types.NewGenesis) {
			fs.chain.WritePreliminaryIntermediateGenesis(b.Header.Height())
		}

		if !b.IdentityDiff.Empty() {
			fs.validators.UpdateFromIdentityStateDiff(b.IdentityDiff)
		}
		fs.chain.WriteIdentityStateDiff(b.Header.Height(), b.IdentityDiff)
		if !b.Cert.Empty() {
			fs.chain.WriteCertificate(b.Header.Hash(), b.Cert, true)
		}
		if b.Header.ProposedHeader == nil || len(b.Header.ProposedHeader.TxBloom) == 0 {
			continue
		}
		bloom, err := common.NewSerializableBFFromData(b.Header.ProposedHeader.TxBloom)
		if err != nil {
			return b.Header.Height(), err
		}
		if fs.testBloom(bloom) {
			txs, err := fs.GetBlockTransactions(b.Header.Hash(), b.Header.ProposedHeader.IpfsHash)
			if err != nil {
				return b.Header.Height(), err
			}
			fs.chain.WriteTxIndex(b.Header.Hash(), txs)
			fs.chain.Indexer().HandleBlockTransactions(b.Header, txs)

			receipts, err := fs.GetTxReceipts(b.Header.ProposedHeader.TxReceiptsCid)
			if err != nil {
				return b.Header.Height(), err
			}
			fs.chain.WriteTxReceipts(b.Header.ProposedHeader.TxReceiptsCid, receipts)
		}
	}
	return 0, nil
}

func (fs *fastSync) testBloom(bloom *common.SerializableBF) bool {
	if bloom.Has(fs.coinBase.Bytes()) {
		return true
	}
	for _, a := range fs.keyStore.Accounts() {
		if bloom.Has(a.Address.Bytes()) {
			return true
		}
	}
	for _, s := range fs.subManager.RawSubscriptions() {
		if bloom.Has(s) {
			return true
		}
	}
	return false
}

func (fs *fastSync) GetBlockTransactions(hash common.Hash, ipfsHash []byte) (types.Transactions, error) {
	if txs, err := fs.ipfs.Get(ipfsHash, ipfs.Block); err != nil {
		return nil, err
	} else {
		if len(txs) > 0 {
			fs.log.Debug("Retrieve block body from ipfs", "hash", hash.Hex())
		}
		body := &types.Body{}
		body.FromBytes(txs)
		return body.Transactions, nil
	}
}

func (fs *fastSync) GetTxReceipts(receiptCid []byte) (types.TxReceipts, error) {
	if data, err := fs.ipfs.Get(receiptCid, ipfs.TxReceipt); err != nil {
		return nil, err
	} else {
		if len(data) == 0 {
			return nil, nil
		}
		body := types.TxReceipts{}
		return body.FromBytes(data), nil
	}
}

func (fs *fastSync) processBatch(batch *batch, attemptNum int) error {
	if fs.manifest == nil {
		panic("manifest is required")
	}
	fs.log.Info("Start process batch", "from", batch.from, "to", batch.to)
	if attemptNum > MaxAttemptsCountPerBatch {
		return errors.New("number of attempts exceeded limit")
	}
	reload := func(from uint64) error {
		b := requestBatch(fs.pm, from, batch.to, batch.p.id)
		if b == nil {
			return errors.New(fmt.Sprintf("batch (%v-%v) can't be loaded", from, batch.to))
		}
		return fs.processBatch(b, attemptNum+1)
	}

	for i := batch.from; i <= batch.to; i++ {
		timeout := time.After(time.Second * 20)

		select {
		case block := <-batch.headers:
			if block == nil {
				err := errors.New("failed to load block header")
				fs.pm.BanPeer(batch.p.id, err)
				return err
			}
			batch.p.resetTimeouts()
			if err := fs.validateHeader(block); err != nil {
				if err == blockchain.ParentHashIsInvalid {
					fs.potentialForkedPeers.Add(batch.p.id)
					return err
				} else {
					fs.pm.BanPeer(batch.p.id, err)
				}
				fs.log.Error("Block header is invalid", "err", err)
				return reload(i)
			}

			fs.deferredHeaders = append(fs.deferredHeaders, blockPeer{*block, batch.p.id})
			if block.Cert != nil && !block.Cert.Empty() {
				if from, err := fs.applyDeferredBlocks(); err != nil {
					return reload(from)
				}
			}

		case <-timeout:
			fs.log.Warn("process batch - timeout was reached", "peer", batch.p.id)
			if batch.p.addTimeout() {
				fs.pm.BanPeer(batch.p.id, BanReasonTimeout)
			}
			return reload(i)
		}
	}
	fs.log.Info("Finish process batch", "from", batch.from, "to", batch.to)
	return nil
}

func (fs *fastSync) validateIdentityState(block blockPeer) error {
	fs.identityStateDB.AddDiff(block.Header.Height(), block.IdentityDiff)
	if fs.identityStateDB.Root() != block.Header.IdentityRoot() {
		fs.identityStateDB.Reset()
		return errors.New("identity root is invalid")
	}
	return nil
}

func (fs *fastSync) validateHeader(block *block) error {
	prevBlock := fs.chain.PreliminaryHead
	if len(fs.deferredHeaders) > 0 {
		prevBlock = fs.deferredHeaders[len(fs.deferredHeaders)-1].Header
	}
	err := fs.chain.ValidateHeader(block.Header, prevBlock)
	if err != nil {
		return err
	}

	if block.Header.Flags().HasFlag(types.IdentityUpdate|types.Snapshot|types.NewGenesis) ||
		block.Header.ProposedHeader != nil && block.Header.ProposedHeader.Upgrade > 0 {
		if block.Cert.Empty() {
			return BlockCertIsMissing
		}
	}
	if !block.Cert.Empty() {
		return fs.chain.ValidateBlockCert(prevBlock, block.Header, block.Cert, fs.validators, fs.pubKeyToAddrCache)
	}

	return nil
}

func (fs *fastSync) postConsuming() error {
	if fs.chain.PreliminaryHead.Height() != fs.manifest.Height {
		return errors.New("preliminary head is lower than manifest's head")
	}

/*	if fs.chain.PreliminaryHead.Root() != fs.manifest.Root {
		fs.sm.AddInvalidManifest(fs.manifest.CidV2)
		return errors.New("preliminary head's root doesn't equal manifest's root")
	}*/
	fs.log.Info("Start loading of snapshot", "height", fs.manifest.Height)
	filePath, version,  err := fs.sm.DownloadSnapshot(fs.manifest)
	if err != nil {
		fs.sm.AddTimeoutManifest(fs.manifest.CidV2)
		return errors.WithMessage(err, "snapshot's downloading has been failed")
	}
	fs.log.Info("Snapshot has been loaded", "height", fs.manifest.Height)

	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	switch version {
	case state.SnapshotVersionV2:
		err = fs.appState.State.RecoverSnapshot2(fs.manifest.Height, fs.chain.PreliminaryHead.Root(), file)
	}

	file.Close()
	if err != nil {
		fs.sm.AddInvalidManifest(fs.manifest.CidV2)
		//TODO : add snapshot to ban list
		return err
	}
	fs.identityStateDB.SaveForcedVersion(fs.chain.PreliminaryHead.Height())

	if err := fs.chain.AtomicSwitchToPreliminary(fs.manifest); err != nil {
		return err
	}

	fs.bus.Publish(events.FastSyncCompletedEvent{})
	return nil
}
