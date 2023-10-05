package cmd

import (
	"context"
	"encoding/json"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/gin-gonic/gin"
	"github.com/strangelove-ventures/noble-cctp-relayer/types"
	"io"
	"net/http"
	"os"
	"strconv"

	"cosmossdk.io/log"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
	"github.com/strangelove-ventures/noble-cctp-relayer/config"
)

var (
	Cfg     config.Config
	cfgFile string
	verbose bool

	Logger log.Logger
)

var rootCmd = &cobra.Command{
	Use:   "noble-cctp-relayer",
	Short: "A CLI tool for relaying CCTP messages",
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		Logger.Error(err.Error())
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "config.yaml", "")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "")

	rootCmd.AddCommand(startCmd)

	cobra.OnInitialize(func() {
		if verbose {
			Logger = log.NewLogger(os.Stdout)
		} else {
			Logger = log.NewLogger(os.Stdout, log.LevelOption(zerolog.InfoLevel))
		}

		Cfg = config.Parse(cfgFile)
		Logger.Info("successfully parsed config file", "location", cfgFile)
		// set defaults

		// if Ethereum start block not set, default to latest
		if Cfg.Networks.Source.Ethereum.StartBlock == 0 {
			client, _ := ethclient.Dial(Cfg.Networks.Source.Ethereum.RPC)
			defer client.Close()
			header, _ := client.HeaderByNumber(context.Background(), nil)
			Cfg.Networks.Source.Ethereum.StartBlock = header.Number.Uint64()
		}

		// if Noble start block not set, default to latest
		if Cfg.Networks.Source.Noble.StartBlock == 0 {
			// todo refactor to use listener's function GetNobleChainTip
			rawResponse, _ := http.Get(Cfg.Networks.Source.Noble.RPC + "/block")
			body, _ := io.ReadAll(rawResponse.Body)
			response := types.BlockResponse{}
			_ = json.Unmarshal(body, &response)
			Cfg.Networks.Source.Noble.StartBlock = uint64(response.Result.Block.Height)
		}

		// start api server
		go startApi()
	})
}

func startApi() {
	gin.SetMode(gin.ReleaseMode)
	router := gin.Default()

	err := router.SetTrustedProxies(Cfg.Api.TrustedProxies) // vpn.primary.strange.love
	if err != nil {
		Logger.Error("unable to set trusted proxies on API server: " + err.Error())
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

	found := false
	var result []types.MessageState
	msgType := c.Query("type") // mint or forward
	if msgType == types.Mint || msgType == "" {
		if message, ok := State.Load(LookupKey(txHash, types.Mint)); ok {
			if domain == "" || (domain != "" && message.SourceDomain == uint32(domainInt)) {
				result = append(result, *message)
				found = true
			}
		}
	}
	if msgType == types.Forward || msgType == "" {
		if message, ok := State.Load(LookupKey(txHash, types.Forward)); ok {
			if domain == "" || (domain != "" && message.SourceDomain == uint32(domainInt)) {
				result = append(result, *message)
				found = true
			}
		}
	}

	if found {
		c.JSON(http.StatusOK, result)
	} else {
		c.JSON(http.StatusNotFound, gin.H{"message": "message not found"})
	}
}
