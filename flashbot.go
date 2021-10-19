// Package flashbot provides a structured way to send TX to the flashbot relays.
// It expects .env file in the root directory that contains all private virables to run the example.
package flashbot

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/big"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/pkg/errors"
)

type Params struct {
	Txs              []string `json:"txs,omitempty"`
	BlockNumber      string   `json:"blockNumber,omitempty"`
	StateBlockNumber string   `json:"stateBlockNumber,omitempty"`
	BundleHash       string   `json:"bundleHash,omitempty"`
}

type Request struct {
	Jsonrpc string   `json:"jsonrpc,omitempty"`
	Id      int      `json:"id,omitempty"`
	Method  string   `json:"method,omitempty"`
	Params  []Params `json:"params,omitempty"`
}

type Metadata struct {
	CoinbaseDiff      string
	EthSentToCoinbase string
	GasFees           string
}

type Result struct {
	BundleGasPrice string
	BundleHash     string
	Metadata
	Results []TxResult
}

type ResultBundleStats struct {
	Error
	Result BundleStats
}

type BundleStats struct {
	IsSimulated    bool
	IsHighPriority bool
	SimulatedAt    time.Time
	SubmittedAt    time.Time
	SentToMinersAt time.Time
}

type TxResult struct {
	Metadata
	FromAddress string
	GasPrice    string
	TxHash      string
	Error       string
	Revert      string
	GasUsed     uint64
}

type Error struct {
	Code    int
	Message string
}

type Response struct {
	Error  `json:"error,omitempty"`
	Result `json:"result,omitempty"`
}

var RequestSend = Request{
	Jsonrpc: "2.0",
	Id:      1,
	Method:  "eth_sendBundle",
	Params:  []Params{{}},
}

var RequestCall = Request{
	Jsonrpc: "2.0",
	Id:      1,
	Method:  "eth_callBundle",
	Params: []Params{
		{
			StateBlockNumber: "latest",
		},
	},
}

var RequestBundleStats = Request{
	Jsonrpc: "2.0",
	Id:      1,
	Method:  "flashbots_getBundleStats",
	Params: []Params{
		{},
	},
}

type Flashbot struct {
	netID      int64
	prvKey     *ecdsa.PrivateKey
	publicAddr *common.Address
	// Url for the relay, when not set it uses the default flashbot url.
	// Making it configurable allows using custom relays (i.e. ethermine).
	url string
}

func New(netID int64, prvKey *ecdsa.PrivateKey, url string) (*Flashbot, error) {
	fb := &Flashbot{
		netID: netID,
		url:   url,
	}

	if prvKey != nil {
		return fb, fb.SetKeys(prvKey)
	}
	return fb, nil

}

func (self *Flashbot) SetKeys(prvKey *ecdsa.PrivateKey) error {
	publicKey := prvKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		return errors.New("casting public key to ECDSA")
	}
	publicAddress := crypto.PubkeyToAddress(*publicKeyECDSA)
	self.prvKey = prvKey
	self.publicAddr = &publicAddress
	return nil
}

func (self *Flashbot) SendBundle(
	txsHex []string,
	blockNumber uint64,
) (*Response, error) {
	r := RequestSend

	r.Params[0].BlockNumber = hexutil.EncodeUint64(blockNumber)
	r.Params[0].Txs = txsHex

	resp, err := self.req(r, self.url)
	if err != nil {
		return nil, errors.Wrap(err, "flashbot send request")
	}

	return parseResp(resp, blockNumber)
}

func (self *Flashbot) CallBundle(
	txsHex []string,
) (*Response, error) {
	r := RequestCall

	blockDummy := uint64(100000000000000)

	r.Params[0].Txs = txsHex
	r.Params[0].BlockNumber = hexutil.EncodeUint64(blockDummy)

	resp, err := self.req(r, self.url)
	if err != nil {
		return nil, errors.Wrap(err, "flashbot call request")
	}

	return parseResp(resp, blockDummy)
}

func (self *Flashbot) GetBundleStats(
	bundleHash string,
	blockNumber uint64,
) (*ResultBundleStats, error) {
	r := RequestBundleStats
	r.Params[0].BundleHash = bundleHash
	r.Params[0].BlockNumber = hexutil.EncodeUint64(blockNumber)

	resp, err := self.req(r, self.url)
	if err != nil {
		return nil, errors.Wrap(err, "flashbot call request")
	}

	rr := &ResultBundleStats{}

	err = json.Unmarshal(resp, rr)
	if err != nil {
		return nil, errors.Wrap(err, "unmarshal flashbot bundle stats response")
	}

	if rr.Error.Code != 0 {
		return nil, errors.Errorf("flashbot request returned an error:%+v,%v", rr.Error, rr.Message)
	}

	return rr, nil

}

func parseResp(resp []byte, blockNum uint64) (*Response, error) {
	rr := &Response{
		Result: Result{},
	}

	err := json.Unmarshal(resp, rr)
	if err != nil {
		return nil, errors.Wrap(err, "unmarshal flashbot call response")
	}

	if rr.Error.Code != 0 || (len(rr.Result.Results) > 0 && rr.Result.Results[0].Error != "") {
		errStr := fmt.Sprintf("flashbot request returned an error:%+v,%v block:%v", rr.Error, rr.Message, blockNum)
		if len(rr.Result.Results) > 0 {
			errStr += fmt.Sprintf(" Result:%+v , Revert:%+v, GasUsed:%+v", rr.Result.Results[0].Error, rr.Result.Results[0].Revert, rr.Result.Results[0].GasUsed)
		}
		return nil, errors.New(errStr)
	}

	return rr, nil
}

func (self *Flashbot) req(r Request, url string) ([]byte, error) {
	payload, err := json.Marshal(r)
	if err != nil {
		return nil, errors.Wrap(err, "marshaling flashbot tx params")
	}
	if url == "" {
		url, err = relayURLDefault(int(self.netID))
		if err != nil {
			return nil, errors.Wrap(err, "get flashboat relay url")
		}
	}
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(payload))
	if err != nil {
		return nil, errors.Wrap(err, "creatting flashbot request")
	}
	signedP, err := self.signPayload(payload)
	if err != nil {
		return nil, errors.Wrap(err, "signing flashbot request")
	}
	req.Header.Add("content-type", "application/json")
	req.Header.Add("Accept", "application/json")
	req.Header.Add("X-Flashbots-Signature", signedP)

	mevHTTPClient := &http.Client{
		Timeout: 3 * time.Second,
	}
	resp, err := mevHTTPClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "flashbot request")
	}
	res, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "reading flashbot reply")
	}

	if resp.StatusCode/100 != 2 {
		rbody, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, errors.Errorf("bad response status %v", resp.Status)
		}
		return nil, errors.Errorf("bad response resp status:%v  respBody:%v reqMethod:%+v", resp.Status, string(rbody)+string(res), r.Method)
	}
	err = resp.Body.Close()
	if err != nil {
		return nil, errors.Wrap(err, "closing flashboat reply body")
	}

	return res, nil
}

func (self *Flashbot) NewSignedTXLegacy(
	data []byte,
	gasLimit uint64,
	gasPrice *big.Int,
	to common.Address,
	nonce uint64,
) (string, *types.Transaction, error) {

	signer := types.LatestSignerForChainID(big.NewInt(self.netID))

	tx, err := types.SignNewTx(self.prvKey, signer, &types.AccessListTx{
		Gas:      gasLimit,
		GasPrice: gasPrice,
		To:       &to,
		ChainID:  big.NewInt(self.netID),
		Nonce:    nonce,
		Data:     data,
	})
	if err != nil {
		return "", nil, errors.Wrap(err, "sign transaction")
	}
	dataM, err := tx.MarshalBinary()
	if err != nil {
		return "", nil, errors.Wrap(err, "marshal tx data")
	}

	return hexutil.Encode(dataM), tx, nil
}

func (self *Flashbot) NewSignedTX(
	data []byte,
	gasLimit uint64,
	gasBaseFee *big.Int,
	gasTip *big.Int,
	to common.Address,
	nonce uint64,
) (string, *types.Transaction, error) {

	signer := types.LatestSignerForChainID(big.NewInt(self.netID))

	tx, err := types.SignNewTx(self.prvKey, signer, &types.DynamicFeeTx{
		ChainID:   big.NewInt(self.netID),
		Nonce:     nonce,
		GasFeeCap: big.NewInt(0).Add(gasBaseFee, gasTip),
		GasTipCap: gasTip,
		Gas:       gasLimit,
		To:        &to,
		Data:      data,
	})
	if err != nil {
		return "", nil, errors.Wrap(err, "sign transaction")
	}
	dataM, err := tx.MarshalBinary()
	if err != nil {
		return "", nil, errors.Wrap(err, "marshal tx data")
	}

	return hexutil.Encode(dataM), tx, nil
}

func (self *Flashbot) signPayload(payload []byte) (string, error) {
	if self.prvKey == nil || self.publicAddr == nil {
		return "", errors.New("private key is not set")
	}
	signature, err := crypto.Sign(
		accounts.TextHash([]byte(hexutil.Encode(crypto.Keccak256(payload)))),
		self.prvKey,
	)
	if err != nil {
		return "", errors.Wrap(err, "sign the payload")
	}

	return self.publicAddr.Hex() +
		":" + hexutil.Encode(signature), nil
}

func relayURLDefault(id int) (string, error) {
	switch id {
	case 1:
		return "https://relay.flashbots.net", nil
	case 5:
		return "https://relay-goerli.flashbots.net", nil
	default:
		return "", errors.Errorf("network id not supported id:%v", id)
	}
}
