package librelay

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"openeth.dev/gen/librelay"
	"openeth.dev/gen/testcontracts"
	"openeth.dev/librelay/txstore"
	"strings"
	"sync"
	"time"

	"code.cloudfoundry.org/clock"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

const TxReceiptTimeout = 60 * time.Second

var lastNonce uint64 = 0
var nonceMutex = &sync.Mutex{}

type RelayTransactionRequest struct {
	EncodedFunction string
	ApprovalData    []byte
	Signature       []byte
	From            common.Address
	To              common.Address
	Paymaster       common.Address
	GasPrice        big.Int
	GasLimit        big.Int
	SenderNonce     big.Int
	RelayMaxNonce   big.Int
	PercentRelayFee big.Int
	BaseRelayFee    big.Int
	RelayHubAddress common.Address
}

type SetHubRequest struct {
	RelayHubAddress common.Address
}

type AuditRelaysRequest struct {
	SignedTx string
}

type GetEthAddrResponse struct {
	RelayServerAddress common.Address
	MinGasPrice        big.Int
	Ready              bool
	Version            string
}

type RelayTransactionResponse struct {
	SignedTx   *types.Transaction
	RawTxBytes []byte
}

func (response *RelayTransactionResponse) MarshalJSON() ([]byte, error) {
	_ = testcontracts.TestSponsor{} // TODO: do not keep, testing build

	return json.Marshal(struct {
		SignedTx   *types.Transaction
		RawTxBytes []byte
	}{
		SignedTx:   response.SignedTx,
		RawTxBytes: types.Transactions{response.SignedTx}.GetRlp(0),
	})
}

type IRelay interface {
	Balance() (balance *big.Int, err error)

	GasPrice() big.Int

	RefreshGasPrice() (err error)

	RegisterRelay() (err error)

	IsStaked() (staked bool, err error)

	IsUnstaked() (removed bool, err error)

	BlockCountSinceLastEvent() (when uint64, err error)

	GetRegistrationBlockRate() (rate uint64)

	IsRemoved() (removed bool, err error)

	SendBalanceToOwner() (err error)

	CreateRelayTransaction(request RelayTransactionRequest) (signedTx *types.Transaction, err error)

	Address() (relayAddress common.Address)

	HubAddress() common.Address

	GetUrl() string

	GetPort() string

	UpdateUnconfirmedTransactions() (newTx *types.Transaction, err error)

	Close() (err error)

	sendRegisterTransaction() (tx *types.Transaction, err error)

	awaitTransactionMined(tx *types.Transaction) (err error)
}

type IClient interface {
	bind.ContractBackend
	ethereum.TransactionReader

	NetworkID(ctx context.Context) (*big.Int, error)

	//From: ChainReader
	BlockByNumber(ctx context.Context, number *big.Int) (*types.Block, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)

	// From:  ChainStateReader, minus CodeAt
	BalanceAt(ctx context.Context, account common.Address, blockNumber *big.Int) (*big.Int, error)
	StorageAt(ctx context.Context, account common.Address, key common.Hash, blockNumber *big.Int) ([]byte, error)
	NonceAt(ctx context.Context, account common.Address, blockNumber *big.Int) (uint64, error)
}

type RelayServer struct {
	OwnerAddress          common.Address
	PercentFee            *big.Int
	BaseFee               *big.Int
	Url                   string
	Port                  string
	RelayHubAddress       common.Address
	DefaultGasPrice       int64
	GasPricePercent       *big.Int
	PrivateKey            *ecdsa.PrivateKey
	RegistrationBlockRate uint64
	EthereumNodeURL       string
	gasPrice              *big.Int // set dynamically as suggestedGasPrice*(GasPricePercent+100)/100
	Client                IClient
	chainID               *big.Int
	TxStore               txstore.ITxStore
	rhub                  *librelay.IRelayHub
	clock                 clock.Clock
	DevMode               bool
}

type RelayParams struct {
	RelayServer
	DBFile string
}

func (relayParams *RelayParams) Dump() {

	log.Println("Relay initial configuration:")
	log.Println("OwnerAddress:", relayParams.OwnerAddress.String())
	log.Println("BaseFee:", relayParams.BaseFee.String())
	log.Println("PercentFee:", relayParams.PercentFee.String())
	log.Println("Url:", relayParams.Url)
	log.Println("Port:", relayParams.Port)
	log.Println("RelayHubAddress:", relayParams.RelayHubAddress.String())
	log.Println("DefaultGasPrice:", relayParams.DefaultGasPrice)
	log.Println("GasPricePercent:", relayParams.GasPricePercent.String())
	log.Println("RegistrationBlockRate:", relayParams.RegistrationBlockRate)
	log.Println("EthereumNodeUrl:", relayParams.EthereumNodeURL)
	if relayParams.DevMode {
		log.Println("Using dev mode")
	}
}

func NewEthClient(EthereumNodeURL string, defaultGasPrice int64) (IClient, error) {
	client := &OpenethClient{DefaultGasPrice: defaultGasPrice}
	var err error
	client.Client, err = ethclient.Dial(EthereumNodeURL)
	return client, err
}

func NewRelayServer(
	OwnerAddress common.Address,
	BaseFee *big.Int,
	PercentFee *big.Int,
	Url string,
	Port string,
	RelayHubAddress common.Address,
	DefaultGasPrice int64,
	GasPricePercent *big.Int,
	PrivateKey *ecdsa.PrivateKey,
	RegistrationBlockRate uint64,
	EthereumNodeURL string,
	Client IClient,
	TxStore txstore.ITxStore,
	clk clock.Clock,
	DevMode bool) (*RelayServer, error) {

	rhub, err := librelay.NewIRelayHub(RelayHubAddress, Client)
	if err != nil {
		return nil, err
	}

	if clk == nil {
		clk = clock.NewClock()
	}

	relay := &RelayServer{
		OwnerAddress:          OwnerAddress,
		PercentFee:            PercentFee,
		BaseFee:               BaseFee,
		Url:                   Url,
		Port:                  Port,
		RelayHubAddress:       RelayHubAddress,
		DefaultGasPrice:       DefaultGasPrice,
		GasPricePercent:       GasPricePercent,
		PrivateKey:            PrivateKey,
		RegistrationBlockRate: RegistrationBlockRate,
		EthereumNodeURL:       EthereumNodeURL,
		Client:                Client,
		TxStore:               TxStore,
		rhub:                  rhub,
		clock:                 clk,
		DevMode:               DevMode,
	}
	return relay, err
}

func (relay *RelayServer) ChainID() (chainID *big.Int, err error) {
	if relay.chainID != nil {
		return relay.chainID, nil
	}

	chainID, err = relay.Client.NetworkID(context.Background())
	if err != nil {
		log.Println("ChainID() failed", err)
		return
	}

	if relay.DevMode && chainID.Int64() < 1000 {
		log.Fatalf("Cowardly refusing to connect to chain with ID=%s in DevMode. Only chains with ID 1000 or higher are supported for dev mode to prevent the relay from being accidentally penalized.", chainID.String())
	}

	relay.chainID = chainID
	return
}

func (relay *RelayServer) Balance() (balance *big.Int, err error) {
	balance, err = relay.Client.BalanceAt(context.Background(), relay.Address(), nil)
	return
}

func (relay *RelayServer) GasPrice() big.Int {
	if relay.gasPrice == nil {
		return *big.NewInt(0)
	}
	return *relay.gasPrice
}

func (relay *RelayServer) RefreshGasPrice() (err error) {
	gasPrice, err := relay.Client.SuggestGasPrice(context.Background())
	if err != nil {
		log.Println("SuggestGasPrice() failed ", err)
		return
	}
	relay.gasPrice = gasPrice.Mul(big.NewInt(0).Add(relay.GasPricePercent, big.NewInt(100)), gasPrice).Div(gasPrice, big.NewInt(100))
	return
}

func (relay *RelayServer) RegisterRelay() (err error) {
	tx, err := relay.sendRegisterTransaction()
	if err != nil {
		return err
	}
	return relay.awaitTransactionMined(tx)
}

func (relay *RelayServer) sendRegisterTransaction() (tx *types.Transaction, err error) {
	desc := fmt.Sprintf("RegisterRelay(address=%s, url=%s)", relay.RelayHubAddress.Hex(), relay.Url)
	tx, err = relay.sendDataTransaction(desc, func(auth *bind.TransactOpts) (*types.Transaction, error) {
		return relay.rhub.RegisterRelay(auth, relay.BaseFee, relay.PercentFee, relay.Url)
	})
	return
}

func (relay *RelayServer) RemoveRelay(ownerKey *ecdsa.PrivateKey) (err error) {
	tx, err := relay.sendRemoveTransaction(ownerKey)
	if err != nil {
		return err
	}
	return relay.awaitTransactionMined(tx)
}

func (relay *RelayServer) sendRemoveTransaction(ownerKey *ecdsa.PrivateKey) (tx *types.Transaction, err error) {
	auth := bind.NewKeyedTransactor(ownerKey)
	desc := fmt.Sprintf("RemoveRelayByOwner(address=%s)", relay.Address())
	log.Println(desc, "tx sending")

	tx, err = relay.rhub.RemoveRelayByOwner(auth, relay.Address())
	if err != nil {
		log.Println(desc, "error sending tx:", err)
		return
	}
	log.Println(desc, "tx sent:", tx.Hash().Hex())
	return
}

func (relay *RelayServer) IsStaked() (staked bool, err error) {
	relayAddress := relay.Address()
	callOpt := &bind.CallOpts{
		From:    relayAddress,
		Pending: false,
	}

	stakeEntry, err := relay.rhub.GetRelay(callOpt, relayAddress)
	if err != nil {
		log.Println(err)
		return
	}
	staked = (stakeEntry.TotalStake.Cmp(big.NewInt(0)) != 0)

	if staked && (relay.OwnerAddress.Hex() == common.HexToAddress("0").Hex()) {
		log.Println("Got staked for the first time, setting owner")
		relay.OwnerAddress = stakeEntry.Owner
		log.Println("Owner is", relay.OwnerAddress.Hex())
		log.Println("Stake:", stakeEntry.TotalStake.String())
	}
	return
}

func (relay *RelayServer) IsUnstaked() (removed bool, err error) {
	filterOpts := &bind.FilterOpts{
		Start: 0,
		End:   nil,
	}
	iter, err := relay.rhub.FilterUnstaked(filterOpts, []common.Address{relay.Address()})
	if err != nil {
		log.Println(err)
		return
	}
	if iter.Event == nil && !iter.Next() {
		return
	}
	return true, nil
}

//find last TransactionRelayed or RelayAdded
func (relay *RelayServer) BlockCountSinceLastEvent() (count uint64, err error) {
	lastBlockHeader, err := relay.Client.HeaderByNumber(context.Background(), nil)
	if err != nil {
		log.Println(err)
		return
	}
	startBlock := uint64(0)
	lastBlockNumber := lastBlockHeader.Number.Uint64()
	if lastBlockNumber > relay.RegistrationBlockRate {
		startBlock = lastBlockHeader.Number.Uint64() - relay.RegistrationBlockRate
	}
	filterOpts := &bind.FilterOpts{
		Start: startBlock,
		End:   &lastBlockNumber,
	}
	iter, err := relay.rhub.FilterRelayAdded(filterOpts, []common.Address{relay.Address()}, nil)
	if err != nil {
		log.Println(err)
		return
	}
	// We only care about the last registration event
	for iter.Next() {
	}
	if (iter.Event == nil && !iter.Next()) ||
		(bytes.Compare(iter.Event.Relay.Bytes(), relay.Address().Bytes()) != 0) ||
		(iter.Event.PctRelayFee.Cmp(relay.PercentFee) != 0) ||
		(iter.Event.Url != relay.Url) {
		return 0, fmt.Errorf("Could not receive RelayAdded events for our relay")
	}
	blockNumber := iter.Event.Raw.BlockNumber

	//Now find also last TransactionRelayed request, and use the latest of these:
	iter1, err1 := relay.rhub.FilterTransactionRelayed(filterOpts, []common.Address{relay.Address()}, nil, nil)
	if err1 != nil {
		log.Println(err1)
		return
	}
	// We only care about the last event
	for iter1.Next() {
	}
	if (iter1.Event != nil &&
		(bytes.Compare(iter1.Event.Relay.Bytes(), relay.Address().Bytes()) == 0) &&
		iter1.Event.Raw.BlockNumber > blockNumber) {
        blockNumber = iter1.Event.Raw.BlockNumber
    }

	count = lastBlockNumber - blockNumber
	return
}

func (relay *RelayServer) GetRegistrationBlockRate() (uint64) {
	return relay.RegistrationBlockRate
}

func (relay *RelayServer) IsRemoved() (removed bool, err error) {
	filterOpts := &bind.FilterOpts{
		Start: 0,
		End:   nil,
	}
	iter, err := relay.rhub.FilterRelayRemoved(filterOpts, []common.Address{relay.Address()})
	if err != nil {
		log.Println(err)
		return
	}
	if iter.Event == nil && !iter.Next() {
		return
	}
	return true, nil
}

func (relay *RelayServer) SendBalanceToOwner() (err error) {
	balance, err := relay.Client.BalanceAt(context.Background(), relay.Address(), nil)
	if err != nil {
		log.Println(err)
		return
	}
	if balance.Cmp(big.NewInt(0)) == 0 {
		log.Println("SendBalanceToOwner: balance is 0")
		return
	}
	log.Println("Sending", balance, "wei to owner address", relay.OwnerAddress.Hex())

	var data []byte
	gasLimit := uint64(21000) // in units
	gasPrice, err := relay.Client.SuggestGasPrice(context.Background())
	if err != nil {
		log.Println(err)
		return
	}
	cost := big.NewInt(int64(gasPrice.Uint64() * gasLimit))
	value := big.NewInt(0)
	value.Sub(balance, cost)

	tx, err := relay.sendPlainTransaction(
		fmt.Sprintf("SendBalanceToOwner(to=%s)", relay.OwnerAddress.Hex()),
		relay.OwnerAddress, value, gasLimit, gasPrice, data,
	)

	if err != nil {
		return
	}
	return relay.awaitTransactionMined(tx)
}

func (relay *RelayServer) CreateRelayTransaction(request RelayTransactionRequest) (signedTx *types.Transaction, err error) {
	// Check that the relayhub is the correct one
	if bytes.Compare(relay.RelayHubAddress.Bytes(), request.RelayHubAddress.Bytes()) != 0 {
		err = fmt.Errorf("Wrong hub address.\nRelay server's hub address: %s, request's hub address: %s\n", relay.RelayHubAddress.Hex(), request.RelayHubAddress.Hex())
		log.Println(err)
		return
	}

	// Check that the fee is acceptable
	if !relay.validateFee(request.PercentRelayFee) {
		err = fmt.Errorf("Unacceptable fee")
		log.Println(err)
		return
	}

	// Check that the gasPrice is initialized & acceptable
	if relay.gasPrice == nil || relay.gasPrice.Cmp(&request.GasPrice) > 0 {
		err = fmt.Errorf("Unacceptable gasPrice")
		log.Println(err)
		return
	}

	if request.RelayMaxNonce.Cmp(big.NewInt(int64(lastNonce))) < 0 {
		err = fmt.Errorf("Unacceptable RelayMaxNonce")
		log.Println(err, request.RelayMaxNonce)
		return
	}
	// canRelay returned true, so we can relay the tx
	relayAddress := relay.Address()

	callOpt := &bind.CallOpts{
		From:    relayAddress,
		Pending: false,
	}

	// With a transition to sponsor-defined gas limits, the server will need to crunch some numbers
	sponsor, err := testcontracts.NewTestSponsor(request.Paymaster, relay.Client)
	if err != nil {
		log.Println(err)
		return
	}

	gasLimits, err := sponsor.GetGasLimits(&bind.CallOpts{From: relayAddress})
	if err != nil {
		log.Println(err)
		return
	}

	hubOverhead, err := relay.rhub.GetHubOverhead(&bind.CallOpts{From: relayAddress})
	if err != nil {
		log.Println(err)
		return
	}
	maxPossibleGas := big.NewInt(0)
	maxPossibleGas.Add(maxPossibleGas, hubOverhead)
	maxPossibleGas.Add(maxPossibleGas, gasLimits.AcceptRelayedCallGasLimit)
	maxPossibleGas.Add(maxPossibleGas, gasLimits.PreRelayedCallGasLimit)
	maxPossibleGas.Add(maxPossibleGas, &request.GasLimit)
	maxPossibleGas.Add(maxPossibleGas, gasLimits.PostRelayedCallGasLimit)

	// TODO: well, this sucks! I need to calculate max possible gas using entire msg.data!!!
	//  In the meantime, these 400'000 extra gas should help :-)
	maxPossibleGas.Add(maxPossibleGas, big.NewInt(400000))
	maxPossibleGas.Add(maxPossibleGas, getEncodedFunctionMaxGas(request.EncodedFunction))
	maxPossibleGas.Add(maxPossibleGas, getEncodedFunctionMaxGas(common.Bytes2Hex(request.ApprovalData)))

	gasData := librelay.GSNTypesGasData{
		GasLimit:        &request.GasLimit,
		GasPrice:        &request.GasPrice,
		PctRelayFee:     &request.PercentRelayFee,
		BaseRelayFee:    &request.BaseRelayFee,
	}

	maxCharge, err := relay.rhub.CalculateCharge(callOpt, maxPossibleGas, gasData)
	if err != nil {
		log.Println(err)
		return
	}

	// check canRelay view function to see if we'll get paid for relaying this tx
	res, err := relay.canRelay(request.From,
		request.To,
		request.Paymaster,
		maxCharge,
		gasLimits.AcceptRelayedCallGasLimit,
		request.EncodedFunction,
		request.PercentRelayFee,
		request.BaseRelayFee,
		request.GasPrice,
		request.GasLimit,
		request.SenderNonce,
		request.Signature,
		request.ApprovalData)

	if err != nil {
		log.Println("canRelay failed in server", err)
		return
	}

	if res.Uint64() != 0 {
		errStr := fmt.Sprintln("EncodedFunction:", request.EncodedFunction, "From:", request.From.Hex(), "To:", request.To.Hex(),
			"GasPrice:", request.GasPrice.String(), "GasLimit:", request.GasLimit.String(), "Nonce:", request.SenderNonce.String(),
			"BaseFee", request.BaseRelayFee.String(),
			"PercentFee:", request.PercentRelayFee.String(),
			"AppData:", hexutil.Encode(request.ApprovalData), "Sig:", hexutil.Encode(request.Signature))
		errStr = errStr[:len(errStr)-1]
		err = fmt.Errorf("canRelay() view function returned error code=%d. params:%s", res, errStr)
		log.Println(err)
		return
	}


	/*
	 * Adding the exact gas cost of the encoded function and approval data as they arethe only dynamic parameters in the relayed call.
	 * While the signature is also byte array, it is checked off chain during canRelay() so any size other than 65 bytes will get reverted on "WrongSignature"
	*/


	sponsorBalance, err := relay.rhub.BalanceOf(callOpt, request.Paymaster)
	if err != nil {
		log.Println(err)
		return
	}

	// Maximum gasLimit of relayed tx consists of:
	// The gas required by a relayed tx consists of:
	// 1. request.GasLimit - user request gasLimit for the relayed function call
	// 2. gasOverhead - Gas cost of all relayCall() instructions before first gasleft() and after last gasleft()
	// 3. gasReserve - Gas cost of all relayCall() instructions after first gasleft() and before last gasleft()
	// 4. acceptRelayedCallMaxGas, postRelayedCallMaxGas, preRelayedCallMaxGas - max gas cost of recipient calls acceptRelayedCall(), postRelayedCall() preRelayedCall()

	if sponsorBalance.Cmp(maxCharge) < 0 {
		err = fmt.Errorf("sponsor balance too low: %d, maxCharge: %d", sponsorBalance, maxCharge)
		log.Println(err)
		return
	}

	log.Println("Estimated max charge of relayed tx:", maxCharge, "GasLimit of relayed tx:", maxPossibleGas)

	signedTx, err = relay.sendDataTransaction(
		fmt.Sprintf("Relay(from=%s, to=%s)", request.From.Hex(), request.To.Hex()),
		func(auth *bind.TransactOpts) (*types.Transaction, error) {
			auth.GasLimit = maxPossibleGas.Uint64()
			auth.GasPrice = &request.GasPrice
			relayRequest := librelay.GSNTypesRelayRequest{
					Target:          request.To,
					EncodedFunction: common.Hex2Bytes(request.EncodedFunction[2:]),
				GasData:  librelay.GSNTypesGasData{
					GasLimit:        &request.GasLimit,
					GasPrice:        &request.GasPrice,
					PctRelayFee:   &request.PercentRelayFee,
					BaseRelayFee:  &request.BaseRelayFee,
				},
				RelayData: librelay.GSNTypesRelayData{
					SenderAddress: request.From,
					SenderNonce:   &request.SenderNonce,
					RelayAddress:  relay.Address(),
					Paymaster:     request.Paymaster,
				},
			}
			return relay.rhub.RelayCall(auth, relayRequest, request.Signature, request.ApprovalData)
		})

	return
}

func (relay *RelayServer) Address() (relayAddress common.Address) {
	publicKey := relay.PrivateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		log.Println(
			"error casting public key to ECDSA")
		return
	}
	relayAddress = crypto.PubkeyToAddress(*publicKeyECDSA)
	return
}

func (relay *RelayServer) HubAddress() common.Address {
	return relay.RelayHubAddress
}

func (relay *RelayServer) GetUrl() string {
	return relay.Url
}

func (relay *RelayServer) GetPort() string {
	return relay.Port
}

func (relay *RelayServer) canRelay(from common.Address,
	to common.Address,
	paymaster common.Address,
	maxPossibleCharge *big.Int,
	acceptRelayedCallMaxGas *big.Int,
	encodedFunction string,
	relayFee big.Int,
	baseRelayFee big.Int,
	gasPrice big.Int,
	gasLimit big.Int,
	senderNonce big.Int,
	signature []byte,
	approvalData []byte) (res *big.Int, err error) {

	relayAddress := relay.Address()

	callOpt := &bind.CallOpts{
		From:    relayAddress,
		Pending: false,
	}

	var result struct {
		Status           *big.Int
		RecipientContext []byte
	}

	relayRequest := librelay.GSNTypesRelayRequest{
		Target:          to,
		EncodedFunction: common.Hex2Bytes(encodedFunction[2:]),
		GasData: librelay.GSNTypesGasData{
			GasLimit:        &gasLimit,
			GasPrice:        &gasPrice,
			PctRelayFee:     &relayFee,
			BaseRelayFee:    &baseRelayFee,
		},
		RelayData: librelay.GSNTypesRelayData{
			SenderAddress: from,
			SenderNonce:   &senderNonce,
			RelayAddress:  relayAddress,
			Paymaster:     paymaster,
		},
	}
	result, err = relay.rhub.CanRelay(callOpt, relayRequest,maxPossibleCharge, acceptRelayedCallMaxGas, signature, approvalData)
	if err != nil {
		log.Println(err)
	} else {
		res = result.Status
	}

	return
}

func (relay *RelayServer) validateFee(relayFee big.Int) bool {
	return relayFee.Cmp(relay.PercentFee) >= 0
}

func (relay *RelayServer) sendPlainTransaction(desc string, to common.Address, value *big.Int, gasLimit uint64, gasPrice *big.Int, data []byte) (signedTx *types.Transaction, err error) {
	log.Println(desc, "tx sending")
	nonceMutex.Lock()
	defer nonceMutex.Unlock()

	nonce, err := relay.pollNonce()
	if err != nil {
		log.Println(desc, "error polling nonce:", err)
		return
	}

	tx := types.NewTransaction(nonce, to, value, gasLimit, gasPrice, data)

	chainID, err := relay.ChainID()
	if err != nil {
		log.Println(desc, "error getting chain id:", err)
		return
	}

	signedTx, err = types.SignTx(tx, types.NewEIP155Signer(chainID), relay.PrivateKey)
	if err != nil {
		log.Println(desc, "error signing tx:", err)
		return
	}

	err = relay.Client.SendTransaction(context.Background(), signedTx)
	if err != nil {
		log.Println(desc, "error sending tx:", err)
		return
	}

	log.Println(desc, "tx sent:", signedTx.Hash().Hex())
	lastNonce++

	err = relay.TxStore.SaveTransaction(signedTx)
	if err != nil {
		log.Println(desc, "error saving tx:", err)
		return
	}

	return
}

func (relay *RelayServer) sendDataTransaction(desc string, f func(*bind.TransactOpts) (*types.Transaction, error)) (tx *types.Transaction, err error) {
	log.Println(desc, "tx sending")
	nonceMutex.Lock()
	defer nonceMutex.Unlock()
	auth := bind.NewKeyedTransactor(relay.PrivateKey)
	nonce, err := relay.pollNonce()
	if err != nil {
		log.Println(desc, "error polling nonce:", err)
		return
	}
	auth.Nonce = big.NewInt(int64(nonce))
	tx, err = f(auth)
	if err != nil {
		log.Println(desc, "error sending tx:", err)
		return
	}

	log.Printf("%v tx sent: %v (%v)\n", desc, tx.Hash().Hex(), tx.Nonce())
	lastNonce++

	// TODO: Monitor for tx mined
	err = relay.TxStore.SaveTransaction(tx)
	if err != nil {
		log.Println(desc, "error saving tx:", err)
		return
	}

	return
}

const maxGasPrice = 100e9
const retryGasPricePercentageIncrease = 20

func (relay *RelayServer) resendTransaction(tx *types.Transaction) (signedTx *types.Transaction, err error) {
	// Calculate new gas price as a % increase over the previous one
	newGasPrice := big.NewInt(100 + retryGasPricePercentageIncrease)
	newGasPrice.Mul(newGasPrice, tx.GasPrice())
	newGasPrice.Div(newGasPrice, big.NewInt(100))

	// Sanity check to ensure we are not burning all our balance in gas fees
	if newGasPrice.Cmp(big.NewInt(maxGasPrice)) > 0 {
		log.Println("Capping gas price to max value of", maxGasPrice)
		newGasPrice.SetUint64(maxGasPrice)
	}

	// Grab chain ID
	chainID, err := relay.ChainID()
	if err != nil {
		return
	}

	// Resend transaction with exactly the same values except for gas price
	newTx := types.NewTransaction(tx.Nonce(), *tx.To(), tx.Value(), tx.Gas(), newGasPrice, tx.Data())
	signedTx, err = types.SignTx(newTx, types.NewEIP155Signer(chainID), relay.PrivateKey)
	if err != nil {
		log.Println("ResendTransaction: error signing tx", err)
		return
	}

	err = relay.Client.SendTransaction(context.Background(), signedTx)
	if err != nil {
		log.Println("ResendTransaction: error sending tx", err)
		return
	}

	return
}

func (relay *RelayServer) awaitTransactionMined(tx *types.Transaction) (err error) {
	start := time.Now()
	var receipt *types.Receipt
	for ; (receipt == nil || err != nil) && time.Since(start) < TxReceiptTimeout; receipt, err = relay.Client.TransactionReceipt(context.Background(), tx.Hash()) {
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		log.Println("Could not get tx receipt", err)
		return
	}
	if receipt.Status != 1 {
		log.Println("tx failed: tx receipt status", receipt.Status)
		return
	}

	return nil
}

func (relay *RelayServer) pollNonce() (nonce uint64, err error) {
	ctx := context.Background()
	fromAddress := relay.Address()
	nonce, err = relay.Client.PendingNonceAt(ctx, fromAddress)
	if err != nil {
		log.Println(err)
		return
	}

	// Always overwrite nonce cache if on dev mode
	if relay.DevMode || lastNonce <= nonce {
		lastNonce = nonce
	} else {
		nonce = lastNonce
	}
	return
}

const confirmationsNeeded = 12
const pendingTransactionTimeout = 5 * 60 // 5 minutes

func (relay *RelayServer) UpdateUnconfirmedTransactions() (newTx *types.Transaction, err error) {
	if relay.DevMode {
		return nil, nil
	}

	// Load unconfirmed transactions from store, and bail if there are none
	tx, err := relay.TxStore.GetFirstTransaction()
	if err != nil {
		log.Println("UpdateUnconfirmedTransactions: error retrieving first transaction from local store", err)
		return
	}

	if tx == nil {
		return
	}

	// Get latest block number in the network
	ctx := context.Background()
	latest, err := relay.Client.HeaderByNumber(ctx, nil)
	if err != nil {
		log.Println("UpdateUnconfirmedTransactions: error retrieving last block number", err)
		return
	}

	// Get nonce at confirmationsNeeded blocks ago
	var confirmedBlock big.Int
	confirmedBlock.Sub(latest.Number, big.NewInt(confirmationsNeeded))
	nonce, err := relay.Client.NonceAt(ctx, relay.Address(), &confirmedBlock)
	if err != nil {
		log.Println("UpdateUnconfirmedTransactions: error retrieving nonce for", relay.Address().Hex(), "on block", confirmedBlock.Uint64(), err)
		return
	}

	// Clear out all confirmed transactions (ie txs with nonce less than the account nonce at confirmationsNeeded blocks ago)
	err = relay.TxStore.RemoveTransactionsLessThanNonce(nonce)
	if err != nil {
		log.Println("UpdateUnconfirmedTransactions: error deleting confirmed transactions", err)
		return
	}

	// Get first unconfirmed transaction
	tx, err = relay.TxStore.GetFirstTransaction()
	if err != nil {
		log.Println("UpdateUnconfirmedTransactions: error retrieving unconfirmed transaction from local store", err)
		return
	}

	if tx == nil {
		return
	}

	// Check if the tx was mined by comparing its nonce against the latest one
	nonce, err = relay.Client.NonceAt(ctx, relay.Address(), nil)
	if err != nil {
		log.Println("UpdateUnconfirmedTransactions: error retrieving nonce for", relay.Address().Hex(), err)
		return
	}

	if tx.Nonce() < nonce {
		log.Println("UpdateUnconfirmedTransactions: awaiting confirmations for next mined transaction", nonce, tx.Nonce(), tx.Hash().Hex())
		return nil, nil
	}

	// If the tx is still pending, check how long ago we sent it, and resend it if needed
	if relay.clock.Now().Unix()-tx.Timestamp < pendingTransactionTimeout {
		log.Println("UpdateUnconfirmedTransactions: awaiting transaction to be mined", nonce, tx.Hash().Hex())
		return
	}

	newtx, err := relay.resendTransaction(tx.Transaction)
	if err != nil {
		log.Println("UpdateUnconfirmedTransactions: error resending transaction", tx.Hash().Hex(), err)
		return nil, err
	}
	log.Println("UpdateUnconfirmedTransactions: resent transaction", tx.Nonce(), tx.Hash().Hex(), "as", newtx.Hash().Hex())

	// TODO: Increase timetamp of subsequent txs?
	err = relay.TxStore.UpdateTransactionByNonce(newtx)
	if err != nil {
		log.Println("UpdateUnconfirmedTransactions: error updating transaction in local store", newtx.Hash().Hex(), err)
		return nil, err
	}

	return newtx, nil
}

func (relay *RelayServer) Close() (err error) {
	return relay.TxStore.Close()
}

/**
 * It is going to be too expensive to run the estimate on-chain, so in the meantime
 * the encoded function cost calculation is going to be a little... Approximate
 */
func getEncodedFunctionMaxGas(encodedFunction string) (*big.Int) {
	return big.NewInt(int64(len(encodedFunction) * 68))
}
/**
 * @return Gas cost of encoded function as parameter in relayedCall
 * As per the yellowpaper, each non-zero byte costs 68 and zero byte costs 4
 */
func getEncodedFunctionGas(encodedFunction string) (*big.Int) {
	if strings.HasPrefix(encodedFunction, "0x") {
		encodedFunction = encodedFunction[2:]
	}
	gasLimitSlack := int64(0)
	for i := 0; i < len(encodedFunction); i += 2 {
		if encodedFunction[i:i+2] == "00" {
			gasLimitSlack += 4
		} else {
			gasLimitSlack += 68
		}
	}
	return big.NewInt(gasLimitSlack)
}
