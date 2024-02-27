package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"cosmossdk.io/log"
	"github.com/gin-gonic/gin"
	"github.com/spf13/cobra"
	"github.com/strangelove-ventures/noble-cctp-relayer/circle"
	"github.com/strangelove-ventures/noble-cctp-relayer/types"
)

// State and Store map the iris api lookup id -> MessageState
// State represents all in progress burns/mints
// Store represents terminal states
var State = types.NewStateMap()

// SequenceMap maps the domain -> the equivalent minter account sequence or nonce
var sequenceMap = types.NewSequenceMap()

func Start(a *AppState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start relaying CCTP transactions",
		Run: func(cmd *cobra.Command, args []string) {
			logger := a.Logger
			cfg := a.Config

			startApi(a)

			// messageState processing queue
			var processingQueue = make(chan *types.TxState, 10000)

			registeredDomains := make(map[types.Domain]types.Chain)

			for name, cfg := range cfg.Chains {
				c, err := cfg.Chain(name)
				if err != nil {
					logger.Error("Error creating chain", "err: ", err)
					os.Exit(1)
				}

				if err := c.InitializeBroadcaster(cmd.Context(), logger, sequenceMap); err != nil {
					logger.Error("Error initializing broadcaster", "err: ", err)
					os.Exit(1)
				}

				go c.StartListener(cmd.Context(), logger, processingQueue)

				if _, ok := registeredDomains[c.Domain()]; ok {
					logger.Error("Duplicate domain found", "domain", c.Domain(), "name:", c.Name())
					os.Exit(1)
				}

				registeredDomains[c.Domain()] = c
			}

			// spin up Processor worker pool
			for i := 0; i < int(cfg.ProcessorWorkerCount); i++ {
				go StartProcessor(cmd.Context(), a, registeredDomains, processingQueue, sequenceMap)
			}

			<-cmd.Context().Done()
		},
	}

	return cmd
}

// StartProcessor is the main processing pipeline.
func StartProcessor(
	ctx context.Context,
	a *AppState,
	registeredDomains map[types.Domain]types.Chain,
	processingQueue chan *types.TxState,
	sequenceMap *types.SequenceMap,
) {
	logger := a.Logger
	cfg := a.Config

	for {
		dequeuedTx := <-processingQueue

		// if this is the first time seeing this message, add it to the State
		State.Mu.Lock()
		tx, ok := State.Load(LookupKey(dequeuedTx.TxHash))
		if !ok {
			State.Store(LookupKey(dequeuedTx.TxHash), dequeuedTx)
			tx, _ = State.Load(LookupKey(dequeuedTx.TxHash))
			for _, msg := range tx.Msgs {
				msg.Status = types.Created
			}
		}
		State.Mu.Unlock()

		var broadcastMsgs = make(map[types.Domain][]*types.MessageState)
		var requeue bool
		for _, msg := range tx.Msgs {

			// if a filter's condition is met, mark as filtered
			if FilterDisabledCCTPRoutes(cfg, logger, msg) ||
				filterInvalidDestinationCallers(registeredDomains, logger, msg) {
				State.Mu.Lock()
				msg.Status = types.Filtered
				State.Mu.Unlock()
			}

			// if the message is burned or pending, check for an attestation
			if msg.Status == types.Created || msg.Status == types.Pending {
				response := circle.CheckAttestation(cfg.Circle.AttestationBaseUrl, logger, msg.IrisLookupId, msg.SourceTxHash, msg.SourceDomain, msg.DestDomain)
				if response != nil {
					if msg.Status == types.Created && response.Status == "pending_confirmations" {
						logger.Debug("Attestation is created but still pending confirmations for 0x" + msg.IrisLookupId + ".  Retrying...")
						State.Mu.Lock()
						msg.Status = types.Pending
						msg.Updated = time.Now()
						State.Mu.Unlock()
						time.Sleep(10 * time.Second)
						requeue = true
						continue
					} else if response.Status == "pending_confirmations" {
						logger.Debug("Attestation is still pending for 0x" + msg.IrisLookupId + ".  Retrying...")
						time.Sleep(10 * time.Second)
						requeue = true
						continue
					} else if response.Status == "complete" {
						logger.Debug("Attestation is complete for 0x" + msg.IrisLookupId + ".  Retrying...")
						State.Mu.Lock()
						msg.Status = types.Attested
						msg.Attestation = response.Attestation
						msg.Updated = time.Now()
						broadcastMsgs[msg.DestDomain] = append(broadcastMsgs[msg.DestDomain], msg)
						State.Mu.Unlock()
					}
				} else {
					// add attestation retry intervals per domain here
					logger.Debug("Attestation is still processing for 0x" + msg.IrisLookupId + ".  Retrying...")
					time.Sleep(10 * time.Second)
					// retry
					requeue = true
					continue
				}
			}
		}
		// if the message is attested to, try to broadcast
		for domain, msgs := range broadcastMsgs {
			chain, ok := registeredDomains[domain]
			if !ok {
				logger.Error("No chain registered for domain", "domain", domain)
				continue
			}

			if err := chain.Broadcast(ctx, logger, msgs, sequenceMap); err != nil {
				logger.Error("unable to mint one or more transfers", "error(s)", err, "total_transfers", len(msgs), "name", chain.Name(), "domain", domain)
				requeue = true
				continue
			}

			State.Mu.Lock()
			for _, msg := range msgs {
				msg.Status = types.Complete
				msg.Updated = time.Now()
			}
			State.Mu.Unlock()

		}
		if requeue {
			processingQueue <- tx
		}
	}
}

// filterDisabledCCTPRoutes returns true if we haven't enabled relaying from a source domain to a destination domain
func FilterDisabledCCTPRoutes(cfg *types.Config, logger log.Logger, msg *types.MessageState) bool {
	val, ok := cfg.EnabledRoutes[msg.SourceDomain]
	if !ok {
		logger.Info(fmt.Sprintf("Filtered tx %s because relaying from %d to %d is not enabled",
			msg.SourceTxHash, msg.SourceDomain, msg.DestDomain))
		return !ok
	}
	for _, dd := range val {
		if dd == msg.DestDomain {
			return false
		}
	}
	logger.Info(fmt.Sprintf("Filtered tx %s because relaying from %d to %d is not enabled",
		msg.SourceTxHash, msg.SourceDomain, msg.DestDomain))
	return true

}

// filterInvalidDestinationCallers returns true if the minter is not the destination caller for the specified domain
func filterInvalidDestinationCallers(registeredDomains map[types.Domain]types.Chain, logger log.Logger, msg *types.MessageState) bool {
	chain, ok := registeredDomains[msg.DestDomain]
	if !ok {
		logger.Error("No chain registered for domain", "domain", msg.DestDomain)
		return true
	}
	validCaller := chain.IsDestinationCaller(msg.DestinationCaller)

	if validCaller {
		// we do not want to filter this message if valid caller
		return false
	}

	logger.Info(fmt.Sprintf("Filtered tx %s from %d to %d due to destination caller: %s)",
		msg.SourceTxHash, msg.SourceDomain, msg.DestDomain, msg.DestinationCaller))
	return true
}

func LookupKey(sourceTxHash string) string {
	// return fmt.Sprintf("%s-%s", sourceTxHash, messageType)
	return sourceTxHash
}

func startApi(a *AppState) {
	logger := a.Logger
	cfg := a.Config
	gin.SetMode(gin.ReleaseMode)
	router := gin.Default()

	err := router.SetTrustedProxies(cfg.Api.TrustedProxies) // vpn.primary.strange.love
	if err != nil {
		logger.Error("unable to set trusted proxies on API server: " + err.Error())
		os.Exit(1)
	}

	router.GET("/tx/:txHash", getTxByHash)
	router.Run("localhost:8000")
}

func getTxByHash(c *gin.Context) {
	txHash := c.Param("txHash")

	domain := c.Query("domain")
	domainInt, err := strconv.ParseInt(domain, 10, 0)
	if domain != "" && err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"message": "unable to parse domain"})
	}

	if tx, ok := State.Load(txHash); ok && domain == "" || (domain != "" && tx.Msgs[0].SourceDomain == types.Domain(domainInt)) {
		c.JSON(http.StatusOK, tx.Msgs)
		return
	}

	c.JSON(http.StatusNotFound, gin.H{"message": "message not found"})
}
