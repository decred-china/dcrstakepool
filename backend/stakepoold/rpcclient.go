package main

import (
	"fmt"
	"io/ioutil"
	"time"

	"github.com/decred-china/dcrstakepool/backend/stakepoold/userdata"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/dcrutil"
	"github.com/decred/dcrd/rpcclient"
)

var requiredChainServerAPI = semver{major: 5, minor: 0, patch: 0}
var requiredWalletAPI = semver{major: 5, minor: 0, patch: 0}

func connectNodeRPC(ctx *appContext, cfg *config) (*rpcclient.Client, semver, error) {
	var nodeVer semver

	dcrdCert, err := ioutil.ReadFile(cfg.DcrdCert)
	if err != nil {
		log.Errorf("Failed to read dcrd cert file at %s: %s\n",
			cfg.DcrdCert, err.Error())
		return nil, nodeVer, err
	}

	log.Infof("Attempting to connect to dcrd RPC %s as user %s "+
		"using certificate located in %s",
		cfg.DcrdHost, cfg.DcrdUser, cfg.DcrdCert)

	connCfgDaemon := &rpcclient.ConnConfig{
		Host:         cfg.DcrdHost,
		Endpoint:     "ws", // websocket
		User:         cfg.DcrdUser,
		Pass:         cfg.DcrdPassword,
		Certificates: dcrdCert,
	}

	ntfnHandlers := getNodeNtfnHandlers(ctx, connCfgDaemon)
	dcrdClient, err := rpcclient.New(connCfgDaemon, ntfnHandlers)
	if err != nil {
		log.Errorf("Failed to start dcrd RPC client: %s\n", err.Error())
		return nil, nodeVer, err
	}

	// Ensure the RPC server has a compatible API version.
	ver, err := dcrdClient.Version()
	if err != nil {
		log.Error("Unable to get RPC version: ", err)
		return nil, nodeVer, fmt.Errorf("Unable to get node RPC version")
	}

	dcrdVer := ver["dcrdjsonrpcapi"]
	nodeVer = semver{dcrdVer.Major, dcrdVer.Minor, dcrdVer.Patch}

	if !semverCompatible(requiredChainServerAPI, nodeVer) {
		return nil, nodeVer, fmt.Errorf("Node JSON-RPC server does not have "+
			"a compatible API version. Advertises %v but require %v",
			nodeVer, requiredChainServerAPI)
	}

	return dcrdClient, nodeVer, nil
}

func connectWalletRPC(cfg *config) (*rpcclient.Client, semver, error) {
	var walletVer semver

	dcrwCert, err := ioutil.ReadFile(cfg.WalletCert)
	if err != nil {
		log.Errorf("Failed to read dcrwallet cert file at %s: %s\n",
			cfg.WalletCert, err.Error())
		return nil, walletVer, err
	}

	log.Infof("Attempting to connect to dcrwallet RPC %s as user %s "+
		"using certificate located in %s",
		cfg.WalletHost, cfg.WalletUser, cfg.WalletCert)

	connCfgWallet := &rpcclient.ConnConfig{
		Host:         cfg.WalletHost,
		Endpoint:     "ws",
		User:         cfg.WalletUser,
		Pass:         cfg.WalletPassword,
		Certificates: dcrwCert,
	}

	ntfnHandlers := getWalletNtfnHandlers(cfg)
	dcrwClient, err := rpcclient.New(connCfgWallet, ntfnHandlers)
	if err != nil {
		log.Errorf("Verify that username and password is correct and that "+
			"rpc.cert is for your wallet: %v", cfg.WalletCert)
		return nil, walletVer, err
	}

	// Ensure the wallet RPC server has a compatible API version.
	ver, err := dcrwClient.Version()
	if err != nil {
		log.Error("Unable to get RPC version: ", err)
		return nil, walletVer, fmt.Errorf("Unable to get node RPC version")
	}

	dcrwVer := ver["dcrwalletjsonrpcapi"]
	walletVer = semver{dcrwVer.Major, dcrwVer.Minor, dcrwVer.Patch}

	if !semverCompatible(requiredWalletAPI, walletVer) {
		log.Warnf("Node JSON-RPC server %v does not have "+
			"a compatible API version. Advertizes %v but require %v",
			cfg.WalletHost, walletVer, requiredWalletAPI)
	}

	return dcrwClient, walletVer, nil
}

func walletGetTickets(ctx *appContext, currentHeight int64) (map[chainhash.Hash]string, map[chainhash.Hash]string) {
	blockHashToHeightCache := make(map[chainhash.Hash]int32)

	// This is suboptimal to copy and needs fixing.
	userVotingConfig := make(map[string]userdata.UserVotingConfig)
	ctx.RLock()
	for k, v := range ctx.userVotingConfig {
		userVotingConfig[k] = v
	}
	ctx.RUnlock()

	ignoredLowFeeTickets := make(map[chainhash.Hash]string)
	liveTickets := make(map[chainhash.Hash]string)
	normalFee := 0

	log.Info("Calling GetTickets...")
	timenow := time.Now()
	tickets, err := ctx.walletConnection.GetTickets(false)
	log.Infof("GetTickets: took %v", time.Since(timenow))

	if err != nil {
		log.Warnf("GetTickets failed: %v", err)
		return ignoredLowFeeTickets, liveTickets
	}

	type promise struct {
		rpcclient.FutureGetTransactionResult
	}
	promises := make([]promise, 0, len(tickets))

	log.Debugf("setting up GetTransactionAsync for %v tickets", len(tickets))
	for _, ticket := range tickets {
		// lookup ownership of each ticket
		promises = append(promises, promise{ctx.walletConnection.GetTransactionAsync(ticket)})
	}

	counter := 0
	for _, p := range promises {
		counter++
		log.Debugf("Receiving GetTransaction result for ticket %v/%v", counter, len(tickets))
		gt, err := p.Receive()
		if err != nil {
			// All tickets should exist and be able to be looked up
			log.Warnf("GetTransaction error: %v", err)
			continue
		}
		for i := range gt.Details {
			_, ok := userVotingConfig[gt.Details[i].Address]
			if !ok {
				log.Warnf("Could not map ticket %v to a user, user %v doesn't exist", gt.TxID, gt.Details[i].Address)
				continue
			}

			addr, err := dcrutil.DecodeAddress(gt.Details[i].Address)
			if err != nil {
				log.Warnf("invalid address %v", err)
				continue
			}

			hash, err := chainhash.NewHashFromStr(gt.TxID)
			if err != nil {
				log.Warnf("invalid ticket %v", err)
				continue
			}

			// All tickets are present in the GetTickets response, whether they
			// pay the correct fee or not.  So we need to verify fees and
			// sort the tickets into their respective maps.
			_, isAdded := ctx.addedLowFeeTicketsMSA[*hash]
			if isAdded {
				liveTickets[*hash] = userVotingConfig[gt.Details[i].Address].MultiSigAddress
			} else {

				msgTx := MsgTxFromHex(gt.Hex)
				if msgTx == nil {
					log.Warnf("MsgTxFromHex failed for %v", gt.Hex)
					continue
				}

				// look up the height at which this ticket was purchased
				var ticketBlockHeight int32
				ticketBlockHash, err := chainhash.NewHashFromStr(gt.BlockHash)
				if err != nil {
					log.Warnf("NewHashFromStr failed for %v: %v", gt.BlockHash, err)
					continue
				}

				height, inCache := blockHashToHeightCache[*ticketBlockHash]
				if inCache {
					ticketBlockHeight = height
				} else {
					gbh, err := ctx.nodeConnection.GetBlockHeader(ticketBlockHash)
					if err != nil {
						log.Warnf("GetBlockHeader failed for %v: %v", ticketBlockHash, err)
						continue
					}

					blockHashToHeightCache[*ticketBlockHash] = int32(gbh.Height)
					ticketBlockHeight = int32(gbh.Height)
				}

				ticketFeesValid, err := evaluateStakePoolTicket(ctx, msgTx, ticketBlockHeight, addr)
				if ticketFeesValid {
					normalFee++
					liveTickets[*hash] = userVotingConfig[gt.Details[i].Address].MultiSigAddress
				} else {
					ignoredLowFeeTickets[*hash] = userVotingConfig[gt.Details[i].Address].MultiSigAddress
					log.Warnf("ignoring ticket %v for msa %v ticketFeesValid %v err %v",
						*hash, ctx.userVotingConfig[gt.Details[i].Address].MultiSigAddress, ticketFeesValid, err)
				}
			}
			break
		}
	}

	log.Infof("tickets loaded -- addedLowFee %v ignoredLowFee %v normalFee %v "+
		"live %v total %v", len(ctx.addedLowFeeTicketsMSA),
		len(ignoredLowFeeTickets), normalFee, len(liveTickets),
		len(tickets))

	return ignoredLowFeeTickets, liveTickets
}
