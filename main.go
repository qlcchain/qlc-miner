package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	rotatelogs "github.com/lestrrat-go/file-rotatelogs"
	rpc "github.com/qlcchain/jsonrpc2"
	"github.com/rifflock/lfshook"
	"github.com/sirupsen/logrus"

	qlcsdk "github.com/qlcchain/qlc-go-sdk"
	"github.com/qlcchain/qlc-go-sdk/pkg/types"
	"github.com/qlcchain/qlc-go-sdk/pkg/util"

	"github.com/qlcchain/qlc-minner/merkle"
)

var flagNodeUrl string
var flagMiner string
var flagAlgo string
var flagAuxPow bool
var flagVersion bool
var flagDebug bool

var extraNonce1 uint32
var extraNonce2 uint64

func main() {
	initLog()

	flag.StringVar(&flagNodeUrl, "nodeurl", "http://127.0.0.1:9735", "RPC URL of node")
	flag.StringVar(&flagMiner, "miner", "", "address of miner account")
	flag.StringVar(&flagAlgo, "algo", "SHA256D", "algo name, such as SHA256D/X11/SCRYPT")
	flag.BoolVar(&flagAuxPow, "auxpow", false, "enable AuxPOW")
	flag.BoolVar(&flagVersion, "version", false, "print version info")
	flag.BoolVar(&flagDebug, "debug", false, "set log level to debug")
	flag.Parse()

	if flagVersion {
		fmt.Println(VersionString())
		return
	}

	if flagDebug {
		logrus.SetLevel(logrus.DebugLevel)
	}

	minerAddr, err := types.HexToAddress(flagMiner)
	if err != nil {
		logrus.Errorln("invalid miner address")
		return
	}

	nodeClient, err := rpc.Dial(flagNodeUrl)
	if err != nil {
		logrus.Errorln("rpc dial error:", err)
		return
	}
	defer nodeClient.Close()

	extraNonce1 = uint32(time.Now().Unix())
	extraNonce2 = uint64(rand.Int63())

	logrus.Infof("running miner, account:%s, algo:%s", minerAddr, flagAlgo)
	logrus.Infof("extraNonce1:%08x, extraNonce2:%16x", extraNonce1, extraNonce2)

	for {
		getWorkRsp := new(qlcsdk.PovApiGetWork)
		err = nodeClient.Call(&getWorkRsp, "pov_getWork", minerAddr, flagAlgo)
		if err != nil {
			logrus.Errorln("getWork error:", err)
			time.Sleep(10 * time.Second)
			continue
		}
		logrus.Infof("getWork response: %s", util.ToString(getWorkRsp))

		var submitWorkReq *qlcsdk.PovApiSubmitWork
		if flagAuxPow {
			submitWorkReq = doWorkByAuxPow(nodeClient, minerAddr, getWorkRsp)
		} else {
			submitWorkReq = doWorkBySelf(nodeClient, minerAddr, getWorkRsp)
		}
		if submitWorkReq == nil {
			time.Sleep(1 * time.Second)
			continue
		}

		logrus.Infof("submitWork request: %s", util.ToString(submitWorkReq))
		err = nodeClient.Call(nil, "pov_submitWork", &submitWorkReq)
		if err != nil {
			logrus.Errorln("submitWork error:", err)
			time.Sleep(10 * time.Second)
			continue
		}
	}
}

func initLog() {
	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		dir = "/tmp"
	}
	fn := dir + "/gqlc-miner.log"

	lw, err := rotatelogs.New(
		fn+".%Y%m%d%H%M",
		rotatelogs.WithLinkName(fn),
	)

	lh := lfshook.NewHook(
		lw,
		&logrus.JSONFormatter{},
	)
	logrus.AddHook(lh)

	logrus.SetLevel(logrus.InfoLevel)
}

func doWorkBySelf(nodeClient *rpc.Client, minerAddr types.Address, getWorkRsp *qlcsdk.PovApiGetWork) *qlcsdk.PovApiSubmitWork {
	povHeader := new(types.PovHeader)
	povHeader.BasHdr.Version = getWorkRsp.Version
	povHeader.BasHdr.Previous = getWorkRsp.Previous
	povHeader.BasHdr.Timestamp = uint32(time.Now().Unix())
	povHeader.BasHdr.Bits = getWorkRsp.Bits

	cbTxExtBuf := new(bytes.Buffer)
	cbTxExtBuf.Write(util.LE_Uint64ToBytes(getWorkRsp.Height))
	cbTxExtBuf.WriteString("/QLC CPU Miner/")
	cbTxExtBuf.Write(util.LE_Uint32ToBytes(extraNonce1))
	cbTxExtBuf.Write(util.LE_Uint64ToBytes(extraNonce2))
	cbTxExtData := cbTxExtBuf.Bytes()

	// hash data = coinbase1 + extra data + coinbase2
	// extra data = minerinfo + extranonce1 + extranonce2
	cbTxDataBuf := new(bytes.Buffer)
	cbTxDataBuf.Write(getWorkRsp.CoinBaseData1)

	cbTxDataBuf.Write(util.LE_EncodeVarInt(uint64(len(cbTxExtData))))
	cbTxDataBuf.Write(cbTxExtData)

	cbTxDataBuf.Write(getWorkRsp.CoinBaseData2)

	cbTxHash := types.Sha256DHashData(cbTxDataBuf.Bytes())

	povHeader.BasHdr.MerkleRoot = merkle.CalcCoinbaseMerkleRoot(&cbTxHash, getWorkRsp.MerkleBranch)

	logrus.Debugf("AlogType: %d, AlgoEfficiency: %d", povHeader.GetAlgoType(), povHeader.GetAlgoEfficiency())

	targetIntAlgo := types.CompactToBig(getWorkRsp.Bits)

	startTm := time.Now()
	lastCheckTm := startTm

	nonce := uint32(rand.Int31())
	tryCnt := 0
	for ; nonce < ^uint32(0); nonce++ {
		povHeader.BasHdr.Nonce = nonce

		powHash := povHeader.ComputePowHash()
		powInt := powHash.ToBigInt()
		if powInt.Cmp(targetIntAlgo) <= 0 {
			logrus.Infof("workHash %s found nonce %d", getWorkRsp.WorkHash, nonce)
			submitWorkReq := new(qlcsdk.PovApiSubmitWork)
			submitWorkReq.WorkHash = getWorkRsp.WorkHash

			submitWorkReq.CoinbaseExtra = cbTxExtBuf.Bytes()
			submitWorkReq.CoinbaseHash = cbTxHash

			submitWorkReq.MerkleRoot = povHeader.BasHdr.MerkleRoot
			submitWorkReq.Timestamp = povHeader.BasHdr.Timestamp
			submitWorkReq.Nonce = povHeader.BasHdr.Nonce

			submitWorkReq.BlockHash = povHeader.ComputeHash()
			return submitWorkReq
		}

		if time.Now().After(lastCheckTm.Add(10 * time.Second)) {
			latestHeader := getLatestHeader(nodeClient)
			if latestHeader != nil && latestHeader.GetHash() != getWorkRsp.Previous {
				logrus.Infof("workHash %s abort search nonce because latest block change", getWorkRsp.WorkHash)
				return nil
			}
			lastCheckTm = time.Now()
		}

		if time.Now().After(startTm.Add(10 * time.Minute)) {
			logrus.Infof("workHash %s abort because exhaust time", getWorkRsp.WorkHash)
			return nil
		}

		tryCnt++
		if tryCnt != 0 && tryCnt%1000000 == 0 {
			logrus.Debugf("workHash %s try compute hash %d", getWorkRsp.WorkHash, tryCnt)
		}
	}

	logrus.Infof("workHash %s abort because exhaust nonce", getWorkRsp.WorkHash)
	return nil
}

func doWorkByAuxPow(nodeClient *rpc.Client, minerAddr types.Address, getWorkRsp *qlcsdk.PovApiGetWork) *qlcsdk.PovApiSubmitWork {
	povHeader := new(types.PovHeader)
	povHeader.BasHdr.Version = getWorkRsp.Version
	povHeader.BasHdr.Previous = getWorkRsp.Previous
	povHeader.BasHdr.Timestamp = uint32(time.Now().Unix())
	povHeader.BasHdr.Bits = getWorkRsp.Bits

	cbTxExtBuf := new(bytes.Buffer)
	cbTxExtBuf.Write(util.LE_Uint64ToBytes(getWorkRsp.Height))
	cbTxExtBuf.WriteString("/QLC CPU AuxPOW/")
	cbTxExtData := cbTxExtBuf.Bytes()

	// hash data = coinbase1 + extra data + coinbase2
	// extra data = minerinfo + extranonce1 + extranonce2
	cbTxDataBuf := new(bytes.Buffer)
	cbTxDataBuf.Write(getWorkRsp.CoinBaseData1)

	cbTxDataBuf.Write(util.LE_EncodeVarInt(uint64(len(cbTxExtData))))
	cbTxDataBuf.Write(cbTxExtData)

	cbTxDataBuf.Write(getWorkRsp.CoinBaseData2)

	cbTxHash := types.Sha256DHashData(cbTxDataBuf.Bytes())

	povHeader.BasHdr.MerkleRoot = merkle.CalcCoinbaseMerkleRoot(&cbTxHash, getWorkRsp.MerkleBranch)

	targetIntAlgo := types.CompactToBig(getWorkRsp.Bits)

	auxBlockHash := povHeader.ComputeHash()
	auxPow := GenerateAuxPow(auxBlockHash)

	startTm := time.Now()
	lastCheckTm := startTm

	nonce := uint32(rand.Int31())
	tryCnt := 0
	for ; nonce < ^uint32(0); nonce++ {
		auxPow.ParBlockHeader.Nonce = nonce

		powHash := auxPow.ComputePowHash(povHeader.GetAlgoType())
		powInt := powHash.ToBigInt()
		if powInt.Cmp(targetIntAlgo) <= 0 {
			logrus.Infof("workHash %s found nonce %d", getWorkRsp.WorkHash, nonce)
			submitWorkReq := new(qlcsdk.PovApiSubmitWork)
			submitWorkReq.WorkHash = getWorkRsp.WorkHash

			submitWorkReq.CoinbaseExtra = cbTxExtBuf.Bytes()
			submitWorkReq.CoinbaseHash = cbTxHash

			submitWorkReq.MerkleRoot = povHeader.BasHdr.MerkleRoot
			submitWorkReq.Timestamp = povHeader.BasHdr.Timestamp
			submitWorkReq.Nonce = povHeader.BasHdr.Nonce

			submitWorkReq.BlockHash = povHeader.ComputeHash()

			auxPow.ParentHash = auxPow.ParBlockHeader.ComputeHash()
			submitWorkReq.AuxPow = auxPow
			return submitWorkReq
		}

		if time.Now().After(lastCheckTm.Add(10 * time.Second)) {
			latestHeader := getLatestHeader(nodeClient)
			if latestHeader != nil && latestHeader.GetHash() != getWorkRsp.Previous {
				logrus.Infof("workHash %s abort search nonce because latest block change", getWorkRsp.WorkHash)
				return nil
			}
			lastCheckTm = time.Now()
		}

		if time.Now().After(startTm.Add(10 * time.Minute)) {
			logrus.Infof("workHash %s abort because exhaust time", getWorkRsp.WorkHash)
			return nil
		}

		tryCnt++
		if tryCnt != 0 && tryCnt%1000000 == 0 {
			logrus.Debugf("workHash %s try compute hash %d", getWorkRsp.WorkHash, tryCnt)
		}
	}

	logrus.Infof("workHash %s abort because exhaust nonce", getWorkRsp.WorkHash)
	return nil
}

func getLatestHeader(nodeClient *rpc.Client) *qlcsdk.PovApiHeader {
	latestHeaderRsp := new(qlcsdk.PovApiHeader)
	err := nodeClient.Call(latestHeaderRsp, "pov_getLatestHeader")
	if err != nil {
		logrus.Errorln("getLatestHeader error:", err)
		return nil
	}
	return latestHeaderRsp
}

func getBtcCoinbase(msgBlockHash types.Hash) *types.PovBtcTx {
	var magic [4]byte           // 4 byte
	var auxBlockHash types.Hash // 32 byte
	var merkleSize int32        // 4 byte
	var merkleNonce int32       // 4 byte

	magic = [4]byte{0xfa, 0xbe, 'm', 'm'}
	auxBlockHash = msgBlockHash
	merkleSize = 1
	merkleNonce = 0

	scriptSig := make([]byte, 0, 4+8+44) // 44 byte
	scriptSigBuf := bytes.NewBuffer(scriptSig)
	binary.Write(scriptSigBuf, binary.LittleEndian, extraNonce1)
	binary.Write(scriptSigBuf, binary.LittleEndian, extraNonce2)
	binary.Write(scriptSigBuf, binary.LittleEndian, magic)
	binary.Write(scriptSigBuf, binary.LittleEndian, auxBlockHash)
	binary.Write(scriptSigBuf, binary.LittleEndian, merkleSize)
	binary.Write(scriptSigBuf, binary.LittleEndian, merkleNonce)

	coinBaseTxin := types.PovBtcTxIn{
		PreviousOutPoint: types.PovBtcOutPoint{
			Hash:  types.ZeroHash,
			Index: types.PovMaxPrevOutIndex,
		},
		SignatureScript: scriptSigBuf.Bytes(),
		Sequence:        types.PovMaxTxInSequenceNum,
	}

	coinBaseTxout := types.PovBtcTxOut{
		Value:    1,
		PkScript: []byte{0x51}, //OP_TRUE
	}

	btcTxin := make([]*types.PovBtcTxIn, 0)
	btcTxin = append(btcTxin, &coinBaseTxin)
	btcTxout := make([]*types.PovBtcTxOut, 0)
	btcTxout = append(btcTxout, &coinBaseTxout)

	coinbase := types.NewPovBtcTx(btcTxin, btcTxout)

	return coinbase
}

func GenerateAuxPow(msgBlockHash types.Hash) *types.PovAuxHeader {
	auxMerkleBranch := make([]*types.Hash, 0)
	auxMerkleIndex := 0
	parCoinbaseTx := getBtcCoinbase(msgBlockHash)
	parCoinBaseMerkle := make([]*types.Hash, 0)
	parMerkleIndex := 0
	parBlockHeader := types.PovBtcHeader{
		Version:    0x7fffffff,
		Previous:   types.ZeroHash,
		MerkleRoot: parCoinbaseTx.ComputeHash(),
		Timestamp:  uint32(time.Now().Unix()),
		Bits:       0, // do not care about parent block diff
		Nonce:      0, // to be solved
	}
	auxPow := &types.PovAuxHeader{
		AuxMerkleBranch:   auxMerkleBranch,
		AuxMerkleIndex:    auxMerkleIndex,
		ParCoinBaseTx:     *parCoinbaseTx,
		ParCoinBaseMerkle: parCoinBaseMerkle,
		ParMerkleIndex:    parMerkleIndex,
		ParBlockHeader:    parBlockHeader,
	}

	return auxPow
}
