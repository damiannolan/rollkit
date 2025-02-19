package node

import (
	"context"
	"fmt"

	"github.com/cometbft/cometbft/libs/log"
	"github.com/cometbft/cometbft/libs/service"
	proxy "github.com/cometbft/cometbft/proxy"
	rpcclient "github.com/cometbft/cometbft/rpc/client"
	cmtypes "github.com/cometbft/cometbft/types"
	ds "github.com/ipfs/go-datastore"
	"github.com/libp2p/go-libp2p/core/crypto"
	"go.uber.org/multierr"

	"github.com/rollkit/rollkit/block"
	"github.com/rollkit/rollkit/config"
	"github.com/rollkit/rollkit/p2p"
	"github.com/rollkit/rollkit/store"
)

var _ Node = &LightNode{}

// LightNode is a rollup node that only needs the header service
type LightNode struct {
	service.BaseService

	P2P *p2p.Client

	proxyApp proxy.AppConns

	hSyncService *block.HeaderSyncService

	client rpcclient.Client

	ctx    context.Context
	cancel context.CancelFunc
}

// GetClient returns a new rpcclient for the light node
func (ln *LightNode) GetClient() rpcclient.Client {
	return ln.client
}

func newLightNode(
	ctx context.Context,
	conf config.NodeConfig,
	p2pKey crypto.PrivKey,
	clientCreator proxy.ClientCreator,
	genesis *cmtypes.GenesisDoc,
	logger log.Logger,
) (*LightNode, error) {
	// Create the proxyApp and establish connections to the ABCI app (consensus, mempool, query).
	proxyApp := proxy.NewAppConns(clientCreator, proxy.NopMetrics())
	proxyApp.SetLogger(logger.With("module", "proxy"))
	if err := proxyApp.Start(); err != nil {
		return nil, fmt.Errorf("error while starting proxy app connections: %v", err)
	}

	datastore, err := openDatastore(conf, logger)
	if err != nil {
		return nil, err
	}
	client, err := p2p.NewClient(conf.P2P, p2pKey, genesis.ChainID, datastore, logger.With("module", "p2p"))
	if err != nil {
		return nil, err
	}

	headerSyncService, err := block.NewHeaderSyncService(ctx, datastore, conf, genesis, client, logger.With("module", "HeaderSyncService"))
	if err != nil {
		return nil, fmt.Errorf("error while initializing HeaderSyncService: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)

	node := &LightNode{
		P2P:          client,
		proxyApp:     proxyApp,
		hSyncService: headerSyncService,
		cancel:       cancel,
		ctx:          ctx,
	}

	node.P2P.SetTxValidator(node.falseValidator())

	node.BaseService = *service.NewBaseService(logger, "LightNode", node)

	node.client = NewLightClient(node)

	return node, nil
}

func openDatastore(conf config.NodeConfig, logger log.Logger) (ds.TxnDatastore, error) {
	if conf.RootDir == "" && conf.DBPath == "" { // this is used for testing
		logger.Info("WARNING: working in in-memory mode")
		return store.NewDefaultInMemoryKVStore()
	}
	return store.NewDefaultKVStore(conf.RootDir, conf.DBPath, "rollkit-light")
}

// Cancel calls the underlying context's cancel function.
func (n *LightNode) Cancel() {
	n.cancel()
}

// OnStart starts the P2P and HeaderSync services
func (ln *LightNode) OnStart() error {
	if err := ln.P2P.Start(ln.ctx); err != nil {
		return err
	}

	if err := ln.hSyncService.Start(); err != nil {
		return fmt.Errorf("error while starting header sync service: %w", err)
	}

	return nil
}

// OnStop stops the light node
func (ln *LightNode) OnStop() {
	ln.Logger.Info("halting light node...")
	ln.cancel()
	err := ln.P2P.Close()
	err = multierr.Append(err, ln.hSyncService.Stop())
	ln.Logger.Error("errors while stopping node:", "errors", err)
}

// Dummy validator that always returns a callback function with boolean `false`
func (ln *LightNode) falseValidator() p2p.GossipValidator {
	return func(*p2p.GossipMessage) bool {
		return false
	}
}
