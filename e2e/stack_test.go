package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	abcitypes "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/config"
	bftclient "github.com/cometbft/cometbft/rpc/client/http"
	bfttypes "github.com/cometbft/cometbft/types"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/polymerdao/monomer/e2e"
	"github.com/polymerdao/monomer/environment"
	"github.com/polymerdao/monomer/node"
	"github.com/polymerdao/monomer/testapp"
	rolluptypes "github.com/polymerdao/monomer/x/rollup/types"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/slog"
)

const (
	artifactsDirectoryName = "artifacts"
	oneEth                 = 1e18
)

func openLogFile(t *testing.T, env *environment.Env, name string) *os.File {
	filename := filepath.Join(artifactsDirectoryName, name+".log")
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_TRUNC|os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	env.DeferErr("close log file: "+filename, file.Close)
	return file
}

func TestE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e tests in short mode")
	}

	env := environment.New()
	defer func() {
		require.NoError(t, env.Close())
	}()

	if err := os.Mkdir(artifactsDirectoryName, 0o755); !errors.Is(err, os.ErrExist) {
		require.NoError(t, err)
	}
	opLogger := log.NewTerminalHandler(openLogFile(t, env, "op"), false)

	prometheusCfg := &config.InstrumentationConfig{
		Prometheus: false,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stack, err := e2e.Setup(ctx, env, prometheusCfg, &e2e.SelectiveListener{
		OPLogCb: func(r slog.Record) {
			require.NoError(t, opLogger.Handle(context.Background(), r))
		},
		NodeSelectiveListener: &node.SelectiveListener{
			OnEngineHTTPServeErrCb: func(err error) {
				require.NoError(t, err)
			},
			OnEngineWebsocketServeErrCb: func(err error) {
				require.NoError(t, err)
			},
			OnCometServeErrCb: func(err error) {
				require.NoError(t, err)
			},
			OnPrometheusServeErrCb: func(err error) {
				require.NoError(t, err)
			},
		},
	})
	require.NoError(t, err)

	l1Client := stack.L1Client
	monomerClient := stack.MonomerClient
	appchainClient := stack.L2Client

	b, err := monomerClient.BlockByNumber(ctx, nil)
	require.NoError(t, err, "monomer block by number")
	l2blockGasLimit := b.GasLimit()

	l1ChainID, err := l1Client.ChainID(ctx)
	require.NoError(t, err, "chain id")

	const targetHeight = 5

	// instantiate L1 user, tx signer.
	user := stack.Users[0]
	l1signer := types.NewEIP155Signer(l1ChainID)

	// send user Deposit Tx
	nonce, err := l1Client.Client.NonceAt(ctx, user.Address, nil)
	require.NoError(t, err)

	gasPrice, err := l1Client.Client.SuggestGasPrice(context.Background())
	require.NoError(t, err)

	l2GasLimit := l2blockGasLimit / 10
	l1GasLimit := l2GasLimit * 2 // must be higher than l2Gaslimit, because of l1 gas burn (cross-chain gas accounting)

	depositTx, err := stack.L1Portal.DepositTransaction(
		&bind.TransactOpts{
			From: user.Address,
			Signer: func(addr common.Address, tx *types.Transaction) (*types.Transaction, error) {
				signed, err := types.SignTx(tx, l1signer, user.PrivateKey)
				if err != nil {
					return nil, err
				}
				return signed, nil
			},
			Nonce:    big.NewInt(int64(nonce)),
			GasPrice: big.NewInt(gasPrice.Int64() * 2),
			GasLimit: l1GasLimit,
			Value:    big.NewInt(oneEth),
			Context:  ctx,
			NoSend:   false,
		},
		user.Address,
		big.NewInt(oneEth/2), // the "minting order" for L2
		l2GasLimit,
		false,    // _isCreation
		[]byte{}, // no data
	)
	require.NoError(t, err, "deposit tx")

	txBytes := testapp.ToTx(t, "userTxKey", "userTxValue")
	bftTx := bfttypes.Tx(txBytes)

	putTx, err := appchainClient.BroadcastTxAsync(ctx, txBytes)
	require.NoError(t, err)
	require.Equal(t, abcitypes.CodeTypeOK, putTx.Code, "put.Code is not OK")
	require.EqualValues(t, bftTx.Hash(), putTx.Hash, "put.Hash does not match local hash")
	t.Log("Monomer can ingest cometbft txs")

	badPutTx := []byte("malformed")
	badPut, err := appchainClient.BroadcastTxAsync(ctx, badPutTx)
	require.NoError(t, err) // no API error - failure encoded in response
	require.NotEqual(t, badPut.Code, abcitypes.CodeTypeOK, "badPut.Code is OK")
	t.Log("Monomer can reject malformed cometbft txs")

	checkTicker := time.NewTicker(250 * time.Millisecond)
	defer checkTicker.Stop()
	for range checkTicker.C {
		block, err := monomerClient.BlockByNumber(ctx, nil)
		require.NoError(t, err)
		if block.NumberU64() >= targetHeight {
			break
		}
	}
	t.Log("Monomer can sync")

	getTx, err := appchainClient.Tx(ctx, bftTx.Hash(), false)

	require.NoError(t, err)
	require.Equal(t, abcitypes.CodeTypeOK, getTx.TxResult.Code, "txResult.Code is not OK")
	require.Equal(t, bftTx, getTx.Tx, "txBytes do not match")
	t.Log("Monomer can serve txs by hash")

	requireEthIsMinted(t, appchainClient)

	txBlock, err := monomerClient.BlockByNumber(ctx, big.NewInt(getTx.Height))
	require.NoError(t, err)
	require.Len(t, txBlock.Transactions(), 2)

	// inspect L1 for deposit tx receipt and emitted TransactionDeposited event
	receipt, err := l1Client.Client.TransactionReceipt(ctx, depositTx.Hash())
	require.NoError(t, err, "deposit tx receipt")
	require.NotNil(t, receipt, "deposit tx receipt")
	require.NotZero(t, receipt.Status, "deposit tx reverted") // receipt.Status == 0 -> reverted tx

	depositLogs, err := stack.L1Portal.FilterTransactionDeposited(
		&bind.FilterOpts{
			Start:   0,
			End:     nil,
			Context: ctx,
		},
		nil, // from any address
		nil, // to any address
		nil, // any event version
	)
	require.NoError(t, err, "configuring 'TransactionDeposited' event listener")
	if !depositLogs.Next() {
		require.FailNowf(t, "finding deposit event", "err: %w", depositLogs.Error())
	}
	require.Equal(t, depositLogs.Event.From, user.Address) // user deposit has emitted L1 event1

	for i := uint64(2); i < targetHeight; i++ {
		block, err := monomerClient.BlockByNumber(ctx, new(big.Int).SetUint64(i))
		require.NoError(t, err)
		txs := block.Transactions()
		require.GreaterOrEqual(t, len(txs), 1, "expected at least 1 tx in block")
		if tx := txs[0]; !tx.IsDepositTx() {
			txBytes, err := tx.MarshalJSON()
			require.NoError(t, err)
			require.Fail(t, fmt.Sprintf("expected tx to be deposit tx: %s", txBytes))
		}
	}
	t.Log("Monomer blocks contain the l1 attributes deposit tx")
}

func requireEthIsMinted(t *testing.T, appchainClient *bftclient.HTTP) {
	query := fmt.Sprintf(
		"%s.%s='%s'",
		rolluptypes.EventTypeMintETH,
		rolluptypes.AttributeKeyL1DepositTxType,
		rolluptypes.L1UserDepositTxType,
	)
	page := 1
	perPage := 100
	orderBy := "desc"

	result, err := appchainClient.TxSearch(
		context.Background(),
		query,
		false,
		&page,
		&perPage,
		orderBy,
	)

	require.NoError(t, err, "search transactions")

	require.NotEmpty(t, result.Txs, "mint_eth event not found")
	t.Log("Monomer can mint_eth from L1 user deposits")
}
