package e2e

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"time"

	cometdb "github.com/cometbft/cometbft-db"
	dbm "github.com/cosmos/cosmos-db"
	opgenesis "github.com/ethereum-optimism/optimism/op-chain-ops/genesis"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/polymerdao/monomer"
	"github.com/polymerdao/monomer/e2e/url"
	"github.com/polymerdao/monomer/environment"
	"github.com/polymerdao/monomer/genesis"
	"github.com/polymerdao/monomer/node"
	"github.com/polymerdao/monomer/testapp"
	"github.com/polymerdao/monomer/testutils"
	rolluptypes "github.com/polymerdao/monomer/x/rollup/types"
)

const oneETH = uint64(1e18)

type EventListener interface {
	OPEventListener
	node.EventListener

	// err will never be nil.
	OnAnvilErr(err error)
}

type Stack struct {
	anvilURL         *url.URL
	monomerEngineURL *url.URL
	monomerCometURL  *url.URL
	opNodeURL        *url.URL
	contractsRootDir string
	eventListener    EventListener
	l1BlockTime      time.Duration
}

// New assumes all ports are available and that all paths exist and are valid.
func New(
	anvilURL,
	monomerEngineURL,
	monomerCometURL,
	opNodeURL *url.URL,
	contractsRootDir string,
	l1BlockTime time.Duration,
	eventListener EventListener,
) *Stack {
	return &Stack{
		anvilURL:         anvilURL,
		monomerEngineURL: monomerEngineURL,
		monomerCometURL:  monomerCometURL,
		opNodeURL:        opNodeURL,
		contractsRootDir: contractsRootDir,
		eventListener:    eventListener,
		l1BlockTime:      l1BlockTime,
	}
}

func (s *Stack) Run(ctx context.Context, env *environment.Env) error {
	// Run anvil.
	anvilCmd := exec.CommandContext( //nolint:gosec
		ctx,
		"anvil",
		"--port", s.anvilURL.Port(),
		"--order", "fifo",
		"--disable-block-gas-limit",
		"--gas-price", "0",
		"--block-time", fmt.Sprint(s.l1BlockTime.Seconds()),
	)
	err := testutils.LogProcess("anvil", anvilCmd)
	if err != nil {
		return fmt.Errorf("log anvil process: %v", err)
	}
	if err := anvilCmd.Start(); err != nil {
		return fmt.Errorf("start %s: %v", anvilCmd, err)
	}
	env.Go(func() {
		if err := anvilCmd.Wait(); err != nil {
			s.eventListener.OnAnvilErr(fmt.Errorf("run %s: %v", anvilCmd, err))
		}
	})
	// NOTE: should we set a timeout on the context? Might not be worth the complexity.
	if !s.anvilURL.IsReachable(ctx) {
		return nil
	}

	// Fund an account.
	anvilRPCClient, err := rpc.DialContext(ctx, s.anvilURL.String())
	if err != nil {
		return fmt.Errorf("dial anvil: %v", err)
	}
	anvil := NewAnvilClient(anvilRPCClient)
	privKey, err := crypto.GenerateKey()
	if err != nil {
		return fmt.Errorf("generate key: %v", err)
	}
	if err := anvil.SetBalance(ctx, crypto.PubkeyToAddress(privKey.PublicKey), 10*oneETH); err != nil { //nolint:gomnd
		return fmt.Errorf("set balance: %v", err)
	}

	// Deploy the OP L1 contracts.
	forgeCmd := exec.CommandContext( //nolint:gosec
		ctx,
		"forge",
		"script",
		"--root", s.contractsRootDir,
		"-vvv",
		fmt.Sprintf("%s:Deploy", filepath.Join(s.contractsRootDir, "scripts", "Deploy.s.sol")),
		"--rpc-url", s.anvilURL.String(),
		"--broadcast",
		"--private-key", common.Bytes2Hex(crypto.FromECDSA(privKey)),
	)
	err = testutils.LogProcess("forge", forgeCmd)
	if err != nil {
		return fmt.Errorf("log forge process: %v", err)
	}
	if err := forgeCmd.Start(); err != nil {
		return fmt.Errorf("start %s: %v", forgeCmd, err)
	}
	if err := forgeCmd.Wait(); err != nil {
		return fmt.Errorf("run %s: %v", forgeCmd, err)
	}
	latestL1Block, err := anvil.BlockByNumber(ctx, nil)
	if err != nil {
		return fmt.Errorf("get the latest l1 block: %v", err)
	}

	// Run Monomer.
	const l2ChainID = 901
	if err := s.runMonomer(ctx, env, latestL1Block.Time(), l2ChainID); err != nil {
		return err
	}
	if !s.monomerEngineURL.IsReachable(ctx) {
		return nil
	}
	monomerRPCClient, err := rpc.DialContext(ctx, s.monomerEngineURL.String())
	if err != nil {
		return fmt.Errorf("dial monomer: %v", err)
	}
	monomerClient := NewMonomerClient(monomerRPCClient)
	l2GenesisBlockHash, err := monomerClient.GenesisHash(ctx)
	if err != nil {
		return fmt.Errorf("get Monomer genesis block hash: %v", err)
	}

	// Get deploy config and rollup config.
	// The Optimism repo only includes configs for Hardhat. Fortunately, Anvil is designed to be compatible and works fine here.
	const networkName = "hardhat"
	l1Deployments, err := opgenesis.NewL1Deployments(filepath.Join(s.contractsRootDir, "deployments", networkName, ".deploy"))
	if err != nil {
		return fmt.Errorf("new l1 deployments: %v", err)
	}
	deployConfig, err := opgenesis.NewDeployConfigWithNetwork(networkName, filepath.Join(s.contractsRootDir, "deploy-config"))
	if err != nil {
		return fmt.Errorf("new deploy config: %v", err)
	}
	deployConfig.L1ChainID = 31337     // The file in the Optimism repo mistakenly sets the Hardhat L1 chain ID to 900.
	deployConfig.L2ChainID = l2ChainID // Ensure Monomer and the deploy config are aligned.
	deployConfig.SetDeployments(l1Deployments)
	rollupConfig, err := deployConfig.RollupConfig(latestL1Block, l2GenesisBlockHash, 1)
	if err != nil {
		return fmt.Errorf("new rollup config: %v", err)
	}

	opStack := NewOPStack(
		s.anvilURL,
		s.monomerEngineURL,
		s.opNodeURL,
		l1Deployments.L2OutputOracleProxy,
		privKey,
		rollupConfig,
		s.eventListener,
	)
	if err := opStack.Run(ctx, env); err != nil {
		return fmt.Errorf("run the op stack: %v", err)
	}
	return nil
}

func (s *Stack) runMonomer(ctx context.Context, env *environment.Env, genesisTime, chainIDU64 uint64) error {
	engineHTTP, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("set up monomer engine http listener: %v", err)
	}
	engineWS, err := net.Listen("tcp", s.monomerEngineURL.Host())
	if err != nil {
		return fmt.Errorf("set up monomer engine ws listener: %v", err)
	}
	cometListener, err := net.Listen("tcp", s.monomerCometURL.Host())
	if err != nil {
		return fmt.Errorf("set up monomer comet listener: %v", err)
	}
	chainID := monomer.ChainID(chainIDU64)
	app, err := testapp.New(dbm.NewMemDB(), chainID.String())
	if err != nil {
		return fmt.Errorf("new test app: %v", err)
	}
	blockdb := dbm.NewMemDB()
	env.DeferErr("close block db", blockdb.Close)
	txdb := cometdb.NewMemDB()
	env.DeferErr("close tx db", txdb.Close)
	mempooldb := dbm.NewMemDB()
	env.DeferErr("close mempool db", mempooldb.Close)
	n := node.New(
		app,
		&genesis.Genesis{
			AppState: app.DefaultGenesis(),
			ChainID:  chainID,
			Time:     genesisTime,
		},
		engineHTTP,
		engineWS,
		cometListener,
		blockdb,
		mempooldb,
		txdb,
		rolluptypes.AdaptCosmosTxsToEthTxs,
		rolluptypes.AdaptPayloadTxsToCosmosTxs,
		s.eventListener,
	)
	if err := n.Run(ctx, env); err != nil {
		return fmt.Errorf("run monomer: %v", err)
	}
	return nil
}
