package main

import "C"

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	wasmtypes "github.com/CosmWasm/wasmd/x/wasm/types"
	abci "github.com/cometbft/cometbft/abci/types"
	tmproto "github.com/cometbft/cometbft/proto/tendermint/types"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/bank/testutil"
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
	"github.com/cosmos/gogoproto/proto"
	"github.com/pkg/errors"

	"github.com/CoreumFoundation/coreum-test-tube/result"
	"github.com/CoreumFoundation/coreum-test-tube/testenv"
)

var (
	envCounter  uint64 = 0
	envRegister        = sync.Map{}
	mu          sync.Mutex
)

//export InitTestEnv
func InitTestEnv() uint64 {
	// Temp fix for concurrency issue
	mu.Lock()
	defer mu.Unlock()

	nodeHome, err := os.MkdirTemp("", ".coreum-test-tube-temp-")
	if err != nil {
		panic(err)
	}

	// set up the validator
	env := new(testenv.TestEnv)
	env.App, env.Validator = testenv.SetupApp(nodeHome)
	env.ParamTypesRegistry = *testenv.NewParamTypeRegistry()

	env.SetupParamTypes()

	// Allow testing unoptimized contract
	wasmtypes.MaxWasmSize = 1024 * 1024 * 1024 * 1024 * 1024

	env.Ctx = env.App.BaseApp.NewContextLegacy(false, tmproto.Header{Height: 0, ChainID: string(testenv.NetworkConfig.ChainID()), Time: time.Now().UTC()})

	validators, err := env.App.StakingKeeper.GetAllValidators(env.Ctx)
	if err != nil {
		panic(err)
	}
	valAddrFancy, _ := validators[0].GetConsAddr()
	err = env.App.SlashingKeeper.SetValidatorSigningInfo(env.Ctx, valAddrFancy, slashingtypes.NewValidatorSigningInfo(
		valAddrFancy,
		0,
		0,
		time.Unix(0, 0),
		false,
		0,
	))
	if err != nil {
		panic(err)
	}

	env.FinalizeBlock(5)
	_, err = env.App.Commit()
	if err != nil {
		panic(err)
	}

	envCounter += 1
	id := envCounter

	envRegister.Store(id, *env)

	return id
}

//export CleanUp
func CleanUp(envId uint64) {
	env := loadEnv(envId)
	if env.NodeHome == "" {
		return
	}
	err := os.RemoveAll(env.NodeHome)
	if err != nil {
		panic(err)
	}
	envRegister.Delete(envId)
}

//export InitAccount
func InitAccount(envId uint64, coinsJson string) *C.char {
	env := loadEnv(envId)
	var coins sdk.Coins

	if err := json.Unmarshal([]byte(coinsJson), &coins); err != nil {
		panic(err)
	}

	priv := secp256k1.GenPrivKey()
	accAddr := sdk.AccAddress(priv.PubKey().Address())

	err := testutil.FundAccount(env.Ctx, env.App.BankKeeper, accAddr, coins)
	if err != nil {
		panic(errors.Wrapf(err, "Failed to fund account"))
	}

	base64Priv := base64.StdEncoding.EncodeToString(priv.Bytes())

	envRegister.Store(envId, env)

	return C.CString(base64Priv)
}

//export IncreaseTime
func IncreaseTime(envId, seconds uint64) {
	env := loadEnv(envId)
	env.FinalizeBlock(seconds)
	envRegister.Store(envId, env)
}

//export FinalizeBlock
func FinalizeBlock(envId uint64) {
	env := loadEnv(envId)
	env.FinalizeBlock(1)
	_, err := env.App.Commit()
	if err != nil {
		panic(err)
	}
	envRegister.Store(envId, env)
}

//export Execute
func Execute(envId uint64, base64ReqDeliverTx string) *C.char {
	env := loadEnv(envId)
	// Temp fix for concurrency issue
	mu.Lock()
	defer mu.Unlock()

	reqDeliverTxBytes, err := base64.StdEncoding.DecodeString(base64ReqDeliverTx)
	if err != nil {
		panic(err)
	}

	_, resDeliverTx, err := env.App.Simulate(reqDeliverTxBytes)
	if err != nil {
		panic(err)
	}
	bz, err := proto.Marshal(resDeliverTx)
	if err != nil {
		panic(err)
	}

	envRegister.Store(envId, env)

	return encodeBytesResultBytes(bz)
}

//export Query
func Query(envId uint64, path, base64QueryMsgBytes string) *C.char {
	env := loadEnv(envId)
	queryMsgBytes, err := base64.StdEncoding.DecodeString(base64QueryMsgBytes)
	if err != nil {
		panic(err)
	}

	req := abci.RequestQuery{}
	req.Data = queryMsgBytes

	route := env.App.GRPCQueryRouter().Route(path)
	if route == nil {
		err := errors.New("No route found for `" + path + "`")
		return encodeErrToResultBytes(result.QueryError, err)
	}
	res, err := route(env.Ctx, &req)
	if err != nil {
		return encodeErrToResultBytes(result.QueryError, err)
	}

	return encodeBytesResultBytes(res.Value)
}

//export GetBlockTime
func GetBlockTime(envId uint64) int64 {
	env := loadEnv(envId)
	return env.Ctx.BlockTime().UnixNano()
}

//export GetBlockHeight
func GetBlockHeight(envId uint64) int64 {
	env := loadEnv(envId)
	return env.Ctx.BlockHeight()
}

//export AccountSequence
func AccountSequence(envId uint64, bech32Address string) uint64 {
	env := loadEnv(envId)

	addr, err := sdk.AccAddressFromBech32(bech32Address)
	if err != nil {
		panic(err)
	}

	seq, err := env.App.AccountKeeper.GetSequence(env.Ctx, addr)
	if err != nil {
		panic(err)
	}

	return seq
}

//export AccountNumber
func AccountNumber(envId uint64, bech32Address string) uint64 {
	env := loadEnv(envId)

	addr, err := sdk.AccAddressFromBech32(bech32Address)
	if err != nil {
		panic(err)
	}

	acc := env.App.AccountKeeper.GetAccount(env.Ctx, addr)
	return acc.GetAccountNumber()
}

//export Simulate
func Simulate(envId uint64, base64TxBytes string) *C.char { // => base64GasInfo
	env := loadEnv(envId)
	// Temp fix for concurrency issue
	mu.Lock()
	defer mu.Unlock()

	txBytes, err := base64.StdEncoding.DecodeString(base64TxBytes)
	if err != nil {
		panic(err)
	}

	gasInfo, _, err := env.App.Simulate(txBytes)
	if err != nil {
		return encodeErrToResultBytes(result.ExecuteError, err)
	}

	bz, err := proto.Marshal(&gasInfo)
	if err != nil {
		panic(err)
	}

	return encodeBytesResultBytes(bz)
}

//export SetParamSet
func SetParamSet(envId uint64, subspaceName, base64ParamSetBytes string) *C.char {
	env := loadEnv(envId)

	// Temp fix for concurrency issue
	mu.Lock()
	defer mu.Unlock()

	paramSetBytes, err := base64.StdEncoding.DecodeString(base64ParamSetBytes)
	if err != nil {
		panic(err)
	}

	subspace, ok := env.App.ParamsKeeper.GetSubspace(subspaceName)
	if !ok {
		err := errors.New("No subspace found for `" + subspaceName + "`")
		return encodeErrToResultBytes(result.ExecuteError, err)
	}

	pReg := env.ParamTypesRegistry

	anyObj := codectypes.Any{}
	err = proto.Unmarshal(paramSetBytes, &anyObj)

	if err != nil {
		return encodeErrToResultBytes(result.ExecuteError, err)
	}

	pset, err := pReg.UnpackAny(&anyObj)
	if err != nil {
		return encodeErrToResultBytes(result.ExecuteError, err)
	}

	subspace.SetParamSet(env.Ctx, pset)

	// return empty bytes if no error
	return encodeBytesResultBytes([]byte{})
}

//export GetParamSet
func GetParamSet(envId uint64, subspaceName, typeUrl string) *C.char {
	env := loadEnv(envId)

	subspace, ok := env.App.ParamsKeeper.GetSubspace(subspaceName)
	if !ok {
		return encodeErrToResultBytes(result.ExecuteError, errors.New("No subspace found for `"+subspaceName+"`"))
	}

	pReg := env.ParamTypesRegistry
	pset, ok := pReg.GetEmptyParamsSet(typeUrl)

	if !ok {
		err := errors.New("No param set found for `" + typeUrl + "`")
		return encodeErrToResultBytes(result.ExecuteError, err)
	}
	subspace.GetParamSet(env.Ctx, pset)

	bz, err := proto.Marshal(pset)
	if err != nil {
		panic(err)
	}

	return encodeBytesResultBytes(bz)
}

//export GetValidatorAddress
func GetValidatorAddress(envId uint64, n int32) *C.char {
	env := loadEnv(envId)
	return C.CString(env.GetValidatorAddresses()[n])
}

//export GetValidatorPrivateKey
func GetValidatorPrivateKey(envId uint64) *C.char {
	env := loadEnv(envId)
	priv := env.GetValidatorPrivateKey()
	base64Priv := base64.StdEncoding.EncodeToString(priv)

	return C.CString(base64Priv)
}

// ========= utils =========

func loadEnv(envId uint64) testenv.TestEnv {
	item, ok := envRegister.Load(envId)
	if !ok {
		panic(fmt.Sprintf("env not found, envId:%d", envId))
	}
	env, ok := item.(testenv.TestEnv)
	if !ok {
		panic(fmt.Sprintf("failed to cast env to testenv.TestEnv, envId:%d", envId))
	}

	return env
}

func encodeErrToResultBytes(code byte, err error) *C.char {
	return C.CString(result.EncodeResultFromError(code, err))
}

func encodeBytesResultBytes(bytes []byte) *C.char {
	return C.CString(result.EncodeResultFromOk(bytes))
}

// must define main for ffi build
func main() {}
