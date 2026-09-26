// Package chain is the backend's thin client for the omnibus contracts:
// deploy from Foundry artifacts, send transactions and wait for receipts,
// read state, read events, and decode custom-error reverts into names.
//
// It binds contracts at runtime from contracts/out/*.json, so there is no
// code generation step between a contract change and the services.
package chain

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// Account is a signing key and its address.
type Account struct {
	Name string
	Key  *ecdsa.PrivateKey
	Addr common.Address
}

// NewAccount generates a fresh key.
func NewAccount(name string) (Account, error) {
	k, err := crypto.GenerateKey()
	if err != nil {
		return Account{}, err
	}
	return Account{Name: name, Key: k, Addr: crypto.PubkeyToAddress(k.PublicKey)}, nil
}

// Client wraps an Ethereum JSON-RPC endpoint.
type Client struct {
	Eth       *ethclient.Client
	RPC       *rpc.Client
	ChainID   *big.Int
	Artifacts string
	errs      map[[4]byte]abi.Error
}

// Dial connects to a node and records where the Foundry artifacts live.
func Dial(ctx context.Context, url, artifacts string) (*Client, error) {
	rc, err := rpc.DialContext(ctx, url)
	if err != nil {
		return nil, err
	}
	ec := ethclient.NewClient(rc)
	id, err := ec.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("chain id: %w", err)
	}
	return &Client{Eth: ec, RPC: rc, ChainID: id, Artifacts: artifacts, errs: map[[4]byte]abi.Error{}}, nil
}

// FundGas gives an account 100 ETH on a development node (anvil).
func (c *Client) FundGas(ctx context.Context, addr common.Address) error {
	wei := new(big.Int).Mul(big.NewInt(100), big.NewInt(1e18))
	return c.RPC.CallContext(ctx, nil, "anvil_setBalance", addr, hexutil.EncodeBig(wei))
}

// SetTime moves the node's clock to t and mines a block there, so contract
// deadlines follow the scenario's calendar.
func (c *Client) SetTime(ctx context.Context, t time.Time) error {
	if err := c.RPC.CallContext(ctx, nil, "evm_setNextBlockTimestamp", t.Unix()); err != nil {
		return err
	}
	return c.RPC.CallContext(ctx, nil, "evm_mine")
}

// Head returns the latest block number.
func (c *Client) Head(ctx context.Context) (uint64, error) {
	return c.Eth.BlockNumber(ctx)
}

type artifact struct {
	ABI      json.RawMessage `json:"abi"`
	Bytecode struct {
		Object string `json:"object"`
	} `json:"bytecode"`
}

func (c *Client) load(name string) (abi.ABI, []byte, error) {
	p := filepath.Join(c.Artifacts, name+".sol", name+".json")
	raw, err := os.ReadFile(p)
	if err != nil {
		return abi.ABI{}, nil, fmt.Errorf("artifact %s: %w (run forge build)", name, err)
	}
	var a artifact
	if err := json.Unmarshal(raw, &a); err != nil {
		return abi.ABI{}, nil, err
	}
	parsed, err := abi.JSON(strings.NewReader(string(a.ABI)))
	if err != nil {
		return abi.ABI{}, nil, err
	}
	for _, e := range parsed.Errors {
		var sel [4]byte
		copy(sel[:], e.ID[:4])
		c.errs[sel] = e
	}
	code, err := hex.DecodeString(strings.TrimPrefix(a.Bytecode.Object, "0x"))
	if err != nil {
		return abi.ABI{}, nil, err
	}
	return parsed, code, nil
}

// Contract is a deployed contract bound to its ABI.
type Contract struct {
	Name  string
	Addr  common.Address
	ABI   abi.ABI
	bound *bind.BoundContract
	c     *Client
}

// Deploy deploys an artifact and waits for it to be mined.
func (c *Client) Deploy(ctx context.Context, from Account, name string, args ...any) (*Contract, error) {
	parsed, code, err := c.load(name)
	if err != nil {
		return nil, err
	}
	opts, err := c.opts(ctx, from)
	if err != nil {
		return nil, err
	}
	addr, tx, bound, err := bind.DeployContract(opts, parsed, code, c.Eth, args...)
	if err != nil {
		return nil, fmt.Errorf("deploy %s: %s", name, c.Explain(err))
	}
	if _, err := bind.WaitDeployed(ctx, c.Eth, tx); err != nil {
		return nil, fmt.Errorf("deploy %s: %w", name, err)
	}
	return &Contract{Name: name, Addr: addr, ABI: parsed, bound: bound, c: c}, nil
}

// At binds an already-deployed contract.
func (c *Client) At(name string, addr common.Address) (*Contract, error) {
	parsed, _, err := c.load(name)
	if err != nil {
		return nil, err
	}
	return &Contract{Name: name, Addr: addr, ABI: parsed, bound: bind.NewBoundContract(addr, parsed, c.Eth, c.Eth, c.Eth), c: c}, nil
}

func (c *Client) opts(ctx context.Context, from Account) (*bind.TransactOpts, error) {
	o, err := bind.NewKeyedTransactorWithChainID(from.Key, c.ChainID)
	if err != nil {
		return nil, err
	}
	o.Context = ctx
	return o, nil
}

// Send submits a transaction and waits for its receipt. A revert comes back
// as an error naming the contract's custom error and its arguments.
func (k *Contract) Send(ctx context.Context, from Account, method string, args ...any) (*types.Receipt, error) {
	opts, err := k.c.opts(ctx, from)
	if err != nil {
		return nil, err
	}
	tx, err := k.bound.Transact(opts, method, args...)
	if err != nil {
		return nil, fmt.Errorf("%s.%s: %s", k.Name, method, k.c.Explain(err))
	}
	rcpt, err := bind.WaitMined(ctx, k.c.Eth, tx)
	if err != nil {
		return nil, err
	}
	if rcpt.Status != types.ReceiptStatusSuccessful {
		return rcpt, fmt.Errorf("%s.%s: reverted in block %d", k.Name, method, rcpt.BlockNumber)
	}
	return rcpt, nil
}

// Call reads a view function.
func (k *Contract) Call(ctx context.Context, method string, args ...any) ([]any, error) {
	var out []any
	if err := k.bound.Call(&bind.CallOpts{Context: ctx}, &out, method, args...); err != nil {
		return nil, fmt.Errorf("%s.%s: %s", k.Name, method, k.c.Explain(err))
	}
	return out, nil
}

// BigInt reads a view returning one uint256.
func (k *Contract) BigInt(ctx context.Context, method string, args ...any) (*big.Int, error) {
	out, err := k.Call(ctx, method, args...)
	if err != nil {
		return nil, err
	}
	return *abi.ConvertType(out[0], new(*big.Int)).(**big.Int), nil
}

// Bool reads a view returning one bool.
func (k *Contract) Bool(ctx context.Context, method string, args ...any) (bool, error) {
	out, err := k.Call(ctx, method, args...)
	if err != nil {
		return false, err
	}
	return out[0].(bool), nil
}

// Events returns this contract's logs of one event in a block range.
func (k *Contract) Events(ctx context.Context, event string, from, to uint64) ([]types.Log, error) {
	ev, ok := k.ABI.Events[event]
	if !ok {
		return nil, fmt.Errorf("%s has no event %s", k.Name, event)
	}
	return k.c.Eth.FilterLogs(ctx, ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(from),
		ToBlock:   new(big.Int).SetUint64(to),
		Addresses: []common.Address{k.Addr},
		Topics:    [][]common.Hash{{ev.ID}},
	})
}

// EventsWithTopic returns this contract's logs of one event whose first
// indexed argument equals topic1.
func (k *Contract) EventsWithTopic(ctx context.Context, event string, topic1 common.Hash, from, to uint64) ([]types.Log, error) {
	ev, ok := k.ABI.Events[event]
	if !ok {
		return nil, fmt.Errorf("%s has no event %s", k.Name, event)
	}
	return k.c.Eth.FilterLogs(ctx, ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(from),
		ToBlock:   new(big.Int).SetUint64(to),
		Addresses: []common.Address{k.Addr},
		Topics:    [][]common.Hash{{ev.ID}, {topic1}},
	})
}

// EventID returns an event's topic-0 hash.
func (k *Contract) EventID(name string) common.Hash {
	return k.ABI.Events[name].ID
}

// Unpack decodes a log into a struct whose exported fields match the
// event's parameter names.
func (k *Contract) Unpack(out any, event string, l types.Log) error {
	return k.bound.UnpackLog(out, event, l)
}

// Explain turns an RPC error carrying revert data into the custom error's
// name and arguments, using every ABI loaded so far.
func (c *Client) Explain(err error) string {
	var de rpc.DataError
	if !errors.As(err, &de) {
		return err.Error()
	}
	s, ok := de.ErrorData().(string)
	if !ok {
		return err.Error()
	}
	data, derr := hexutil.Decode(s)
	if derr != nil || len(data) < 4 {
		return err.Error()
	}
	var sel [4]byte
	copy(sel[:], data[:4])
	e, ok := c.errs[sel]
	if !ok {
		return fmt.Sprintf("%s (selector 0x%x)", err.Error(), sel)
	}
	vals, uerr := e.Inputs.Unpack(data[4:])
	if uerr != nil {
		return e.Name
	}
	parts := make([]string, len(vals))
	for i, v := range vals {
		switch x := v.(type) {
		case [32]byte:
			parts[i] = strings.TrimRight(string(x[:]), "\x00")
			if !isPrintable(parts[i]) {
				parts[i] = "0x" + hex.EncodeToString(x[:])
			}
		default:
			parts[i] = fmt.Sprint(v)
		}
	}
	return fmt.Sprintf("reverted %s(%s)", e.Name, strings.Join(parts, ", "))
}

func isPrintable(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

// Bytes32 left-aligns a short ASCII string, the way Solidity literals do.
func Bytes32(s string) [32]byte {
	var b [32]byte
	copy(b[:], s)
	return b
}

// RefOf hashes an external reference (a UETR, a core posting id) into the
// bytes32 the contracts use for idempotency.
func RefOf(s string) [32]byte {
	return crypto.Keccak256Hash([]byte(s))
}

// Bytes4 packs an ISO reason code.
func Bytes4(s string) [4]byte {
	var b [4]byte
	copy(b[:], s)
	return b
}
