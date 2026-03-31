package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	cometdbm "github.com/cometbft/cometbft-db"
	"github.com/cometbft/cometbft/crypto"
	tmd25519 "github.com/cometbft/cometbft/crypto/ed25519"
	cmtjson "github.com/cometbft/cometbft/libs/json"
	cmtstate "github.com/cometbft/cometbft/proto/tendermint/state"
	tmproto "github.com/cometbft/cometbft/proto/tendermint/types"
	sm "github.com/cometbft/cometbft/state"
	"github.com/cometbft/cometbft/store"
	tmtypes "github.com/cometbft/cometbft/types"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/spf13/cast"
	"github.com/spf13/cobra"

	"cosmossdk.io/log"
	"cosmossdk.io/math"
	upgradetypes "cosmossdk.io/x/upgrade/types"

	"github.com/cosmos/cosmos-sdk/client/flags"
	"github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/crypto/keys/ed25519"
	"github.com/cosmos/cosmos-sdk/server"
	servertypes "github.com/cosmos/cosmos-sdk/server/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	distrtypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	genutiltypes "github.com/cosmos/cosmos-sdk/x/genutil/types"
	minttypes "github.com/cosmos/cosmos-sdk/x/mint/types"
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	"github.com/CosmWasm/wasmd/app"
	wasmkeeper "github.com/CosmWasm/wasmd/x/wasm/keeper"
	wasmtypes "github.com/CosmWasm/wasmd/x/wasm/types"
)

const (
	flagValidatorOperatorAddress = "validator-operator"
	flagValidatorPubKey          = "validator-pubkey"
	flagValidatorPrivKey         = "validator-privkey"
	flagAccountsToFund           = "accounts-to-fund"
	flagCoinsToFund              = "coins-to-fund"
	flagUpgradeHeight            = "upgrade-height"
	flagUpgradeName              = "upgrade-name"
	flagCosmWasmAdmin            = "cosmwasm-admin"

	valVotingPower int64 = 900000000000000
	valTokens      int64 = 1000000000000000
)

type valArgs struct {
	validatorOperatorAddress string
	validatorConsPubKeyByte  []byte
	validatorConsPrivKey     crypto.PrivKey
	accountsToFund           []sdk.AccAddress
	coinsToFund              sdk.Coins
	homeDir                  string
	upgradeHeight            int64
	upgradeName              string
	cosmwasmAdmin            string
	newChainID               string
}

const flagNewChainID = "new-chain-id"

// InPlaceTestnetCmd returns the cobra command for creating an in-place testnet.
func InPlaceTestnetCmd() *cobra.Command {
	cmd := server.StartCmd(newTestnetApp, app.DefaultNodeHome)
	cmd.Use = "in-place-testnet [newChainID]"
	cmd.Short = "Updates chain's application and consensus state with provided validator info and starts the node"
	cmd.Long = `The in-place-testnet command modifies both application and consensus stores within a local node and starts the node.
This command replaces the existing validator set with a single new validator, enabling local testing from mainnet state.

The validator consensus key is read from priv_validator_key.json (set during lumend init or manually).
The validator operator address is provided via --validator-operator.

Example:
	lumend in-place-testnet testing --validator-operator=euclidvaloper1... --accounts-to-fund=euclid1... --cosmwasm-admin=euclid1...
	`
	cmd.Args = cobra.ExactArgs(1)

	// Wrap the existing RunE to inject the chain ID positional arg into viper
	originalRunE := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		serverCtx := server.GetServerContextFromCmd(cmd)
		serverCtx.Viper.Set(flagNewChainID, args[0])
		return originalRunE(cmd, args)
	}

	cmd.Flags().String(flagValidatorOperatorAddress, "", "Validator operator address (e.g. euclidvaloper1...)")
	cmd.Flags().String(flagValidatorPubKey, "", "Validator consensus public key (base64 Ed25519, from priv_validator_key.json)")
	cmd.Flags().String(flagValidatorPrivKey, "", "Validator consensus private key (base64 Ed25519, from priv_validator_key.json)")
	cmd.Flags().String(flagAccountsToFund, "", "Comma-separated list of account addresses to fund")
	cmd.Flags().String(flagCoinsToFund, "", "Coins per funded account (e.g. 1000000000ualpha)")
	cmd.Flags().Int64(flagUpgradeHeight, 0, "Number of blocks from current height at which to schedule a software upgrade")
	cmd.Flags().String(flagUpgradeName, "", "Name of the upgrade to schedule")
	cmd.Flags().String(flagCosmWasmAdmin, "", "Override admin of all CosmWasm contracts to this address")

	return cmd
}

func getCommandArgs(appOpts servertypes.AppOptions) (valArgs, error) {
	args := valArgs{}

	valoperAddress := cast.ToString(appOpts.Get(flagValidatorOperatorAddress))
	if valoperAddress == "" {
		return args, errors.New("--validator-operator is required")
	}
	if _, err := sdk.ValAddressFromBech32(valoperAddress); err != nil {
		return args, fmt.Errorf("invalid validator operator address: %w", err)
	}
	args.validatorOperatorAddress = valoperAddress

	validatorPubKey := cast.ToString(appOpts.Get(flagValidatorPubKey))
	if validatorPubKey == "" {
		return args, errors.New("--validator-pubkey is required")
	}
	decPubKey, err := base64.StdEncoding.DecodeString(validatorPubKey)
	if err != nil {
		return args, fmt.Errorf("cannot decode validator pubkey: %w", err)
	}
	args.validatorConsPubKeyByte = decPubKey

	validatorPrivKey := cast.ToString(appOpts.Get(flagValidatorPrivKey))
	if validatorPrivKey == "" {
		return args, errors.New("--validator-privkey is required")
	}
	decPrivKey, err := base64.StdEncoding.DecodeString(validatorPrivKey)
	if err != nil {
		return args, fmt.Errorf("cannot decode validator private key: %w", err)
	}
	args.validatorConsPrivKey = tmd25519.PrivKey(decPrivKey)

	accountsString := cast.ToString(appOpts.Get(flagAccountsToFund))
	for _, account := range strings.Split(accountsString, ",") {
		account = strings.TrimSpace(account)
		if account != "" {
			addr, err := sdk.AccAddressFromBech32(account)
			if err != nil {
				return args, fmt.Errorf("invalid account address %q: %w", account, err)
			}
			args.accountsToFund = append(args.accountsToFund, addr)
		}
	}

	coinsString := cast.ToString(appOpts.Get(flagCoinsToFund))
	if coinsString != "" {
		coins, err := sdk.ParseCoinsNormalized(coinsString)
		if err != nil {
			return args, fmt.Errorf("invalid coins format: %w", err)
		}
		args.coinsToFund = coins
	}

	upgradeHeight := cast.ToInt64(appOpts.Get(flagUpgradeHeight))
	upgradeName := cast.ToString(appOpts.Get(flagUpgradeName))
	if upgradeHeight > 0 && upgradeName == "" {
		return args, errors.New("--upgrade-name is required when --upgrade-height is set")
	}
	if upgradeName != "" && upgradeHeight <= 0 {
		return args, errors.New("--upgrade-height must be positive when --upgrade-name is set")
	}
	args.upgradeHeight = upgradeHeight
	args.upgradeName = upgradeName

	args.cosmwasmAdmin = cast.ToString(appOpts.Get(flagCosmWasmAdmin))
	args.newChainID = cast.ToString(appOpts.Get(flagNewChainID))

	homeDir := cast.ToString(appOpts.Get(flags.FlagHome))
	if homeDir == "" {
		return args, errors.New("invalid home dir")
	}
	args.homeDir = homeDir

	return args, nil
}

func newTestnetApp(
	logger log.Logger,
	db dbm.DB,
	traceStore io.Writer,
	appOpts servertypes.AppOptions,
) servertypes.Application {
	wasmApp := newApp(logger, db, traceStore, appOpts).(*app.WasmApp)

	args, err := getCommandArgs(appOpts)
	if err != nil {
		panic(err)
	}

	// Update consensus state (CometBFT state DB + block store)
	if err := updateConsensusState(logger, appOpts, wasmApp.CommitMultiStore().LatestVersion(), args); err != nil {
		panic(fmt.Sprintf("failed to update consensus state: %v", err))
	}

	// Update application state
	if err := updateApplicationState(wasmApp, args); err != nil {
		panic(fmt.Sprintf("failed to update application state: %v", err))
	}

	return wasmApp
}

// updateConsensusState modifies the CometBFT state and block store.
// This is adapted from the SDK's testnetify function, with the genesis doc
// chain ID bug fixed and extended commit handling added for CometBFT v0.38.
func updateConsensusState(logger log.Logger, appOpts servertypes.AppOptions, appHeight int64, args valArgs) error {
	newTmVal := tmtypes.NewValidator(tmd25519.PubKey(args.validatorConsPubKeyByte), valVotingPower)
	newValSet := &tmtypes.ValidatorSet{
		Validators: []*tmtypes.Validator{newTmVal},
		Proposer:   newTmVal,
	}

	// Update genesis file chain ID
	if args.newChainID != "" {
		genFilePath := filepath.Join(args.homeDir, "config", "genesis.json")
		appGenesis, err := genutiltypes.AppGenesisFromFile(genFilePath)
		if err != nil {
			return fmt.Errorf("failed to read genesis file: %w", err)
		}
		appGenesis.ChainID = args.newChainID
		if err := appGenesis.ValidateAndComplete(); err != nil {
			return fmt.Errorf("failed to validate genesis: %w", err)
		}
		if err := appGenesis.SaveAs(genFilePath); err != nil {
			return fmt.Errorf("failed to save genesis: %w", err)
		}
	}

	// Open state DB
	stateDB, err := openDB(args.homeDir, "state", cometdbm.BackendType(server.GetAppDBBackend(appOpts)))
	if err != nil {
		return fmt.Errorf("failed to open state db: %w", err)
	}
	defer stateDB.Close()

	stateStore := sm.NewStore(stateDB, sm.StoreOptions{DiscardABCIResponses: false})
	state, err := stateStore.Load()
	if err != nil {
		return fmt.Errorf("failed to load state: %w", err)
	}

	// Open block store
	blockStoreDB, err := openDB(args.homeDir, "blockstore", cometdbm.BackendType(server.GetAppDBBackend(appOpts)))
	if err != nil {
		return fmt.Errorf("failed to open blockstore db: %w", err)
	}
	defer blockStoreDB.Close()

	blockStore := store.NewBlockStore(blockStoreDB)

	// Handle height mismatches (adapted from SDK testnetify)
	switch {
	case appHeight == blockStore.Height():
		if state.LastBlockHeight != appHeight {
			state.LastBlockHeight = appHeight
		} else {
			// Delete next block's seen commit to prevent issues
			_ = blockStoreDB.Delete([]byte(fmt.Sprintf("SC:%v", blockStore.Height()+1)))
		}
	case blockStore.Height() > state.LastBlockHeight:
		if err := blockStore.DeleteLatestBlock(); err != nil {
			return fmt.Errorf("failed to delete latest block: %w", err)
		}
	}

	// Update chain ID
	if args.newChainID != "" {
		state.ChainID = args.newChainID
	}

	// Re-sign the last commit with the new validator
	lastCommit := blockStore.LoadSeenCommit(state.LastBlockHeight)
	var vote *tmtypes.Vote
	for idx, commitSig := range lastCommit.Signatures {
		if commitSig.BlockIDFlag == tmtypes.BlockIDFlagAbsent {
			continue
		}
		vote = lastCommit.GetVote(int32(idx))
		break
	}
	if vote == nil {
		return errors.New("cannot get vote from last commit")
	}

	voteSignBytes := tmtypes.VoteSignBytes(state.ChainID, vote.ToProto())
	signatureBytes, err := args.validatorConsPrivKey.Sign(voteSignBytes)
	if err != nil {
		return fmt.Errorf("failed to sign vote: %w", err)
	}

	newCommitSig := tmtypes.CommitSig{
		BlockIDFlag:      tmtypes.BlockIDFlagCommit,
		ValidatorAddress: newTmVal.Address,
		Timestamp:        vote.Timestamp,
		Signature:        signatureBytes,
	}

	lastCommit.Signatures = []tmtypes.CommitSig{newCommitSig}
	if err := blockStore.SaveSeenCommit(state.LastBlockHeight, lastCommit); err != nil {
		return fmt.Errorf("failed to save seen commit: %w", err)
	}

	// Update extended commit for CometBFT v0.38 (ABCI 2.0).
	// Even with vote extensions disabled, CometBFT reconstructs an ExtendedCommit
	// from the SeenCommit and validates addresses against the validator set.
	// We write the extended commit at the same height to keep them in sync.
	newExtCommit := &tmtypes.ExtendedCommit{
		Height:  state.LastBlockHeight,
		Round:   lastCommit.Round,
		BlockID: lastCommit.BlockID,
		ExtendedSignatures: []tmtypes.ExtendedCommitSig{{
			CommitSig:          newCommitSig,
			Extension:          []byte{},
			ExtensionSignature: []byte{},
		}},
	}
	pbec := newExtCommit.ToProto()
	extCommitBytes, err := pbec.Marshal()
	if err != nil {
		return fmt.Errorf("failed to marshal extended commit: %w", err)
	}
	if err := blockStoreDB.Set([]byte(fmt.Sprintf("EC:%v", state.LastBlockHeight)), extCommitBytes); err != nil {
		return fmt.Errorf("failed to save extended commit: %w", err)
	}

	// Update validator set in state
	state.LastValidators = newValSet
	state.Validators = newValSet
	state.NextValidators = newValSet
	state.LastHeightValidatorsChanged = state.LastBlockHeight

	if err := stateStore.Save(state); err != nil {
		return fmt.Errorf("failed to save state: %w", err)
	}

	// Save validators info for current and adjacent heights
	protoValSet, err := newValSet.ToProto()
	if err != nil {
		return fmt.Errorf("failed to convert validator set: %w", err)
	}
	valInfo := &cmtstate.ValidatorsInfo{
		ValidatorSet:      protoValSet,
		LastHeightChanged: state.LastBlockHeight,
	}
	for _, h := range []int64{state.LastBlockHeight - 1, state.LastBlockHeight, state.LastBlockHeight + 1} {
		if err := saveValidatorsInfo(stateDB, h, valInfo); err != nil {
			return fmt.Errorf("failed to save validators info at height %d: %w", h, err)
		}
	}

	// Update genesis doc in state DB with correct chain ID
	if args.newChainID != "" {
		genFilePath := filepath.Join(args.homeDir, "config", "genesis.json")
		appGenesis, err := genutiltypes.AppGenesisFromFile(genFilePath)
		if err != nil {
			return fmt.Errorf("failed to read genesis for state db: %w", err)
		}
		genDoc, err := appGenesis.ToGenesisDoc()
		if err != nil {
			return fmt.Errorf("failed to convert genesis doc: %w", err)
		}
		b, err := cmtjson.Marshal(genDoc)
		if err != nil {
			return fmt.Errorf("failed to marshal genesis doc: %w", err)
		}
		if err := stateDB.SetSync([]byte("genesisDoc"), b); err != nil {
			return fmt.Errorf("failed to save genesis doc to state db: %w", err)
		}
	}

	logger.Info("Updated consensus state",
		"validator", fmt.Sprintf("%X", newTmVal.Address),
		"height", state.LastBlockHeight,
		"chain_id", state.ChainID,
	)

	return nil
}

// updateApplicationState modifies the application state.
func updateApplicationState(wasmApp *app.WasmApp, args valArgs) error {
	pubkey := &ed25519.PubKey{Key: args.validatorConsPubKeyByte}
	pubkeyAny, err := types.NewAnyWithValue(pubkey)
	if err != nil {
		return err
	}

	ctx := wasmApp.NewUncachedContext(true, tmproto.Header{})

	// STAKING — replace validator set
	newVal := stakingtypes.Validator{
		OperatorAddress: args.validatorOperatorAddress,
		ConsensusPubkey: pubkeyAny,
		Jailed:          false,
		Status:          stakingtypes.Bonded,
		Tokens:          math.NewInt(valTokens),
		DelegatorShares: math.LegacyMustNewDecFromStr(fmt.Sprintf("%d", valTokens)),
		Description:     stakingtypes.Description{Moniker: "testnet-validator"},
		Commission: stakingtypes.Commission{
			CommissionRates: stakingtypes.CommissionRates{
				Rate:          math.LegacyMustNewDecFromStr("0.05"),
				MaxRate:       math.LegacyMustNewDecFromStr("0.1"),
				MaxChangeRate: math.LegacyMustNewDecFromStr("0.05"),
			},
		},
		MinSelfDelegation: math.OneInt(),
	}

	newValAddr, err := wasmApp.StakingKeeper.ValidatorAddressCodec().StringToBytes(newVal.GetOperator())
	if err != nil {
		return err
	}

	// Delete all existing validators
	validators, err := wasmApp.StakingKeeper.GetAllValidators(ctx)
	if err != nil {
		return fmt.Errorf("failed to get validators: %w", err)
	}
	for _, v := range validators {
		valAddr, err := wasmApp.StakingKeeper.ValidatorAddressCodec().StringToBytes(v.GetOperator())
		if err != nil {
			continue
		}
		valConsAddr, err := v.GetConsAddr()
		if err != nil {
			continue
		}
		kvStore := ctx.KVStore(wasmApp.GetKey(stakingtypes.ModuleName))
		kvStore.Delete(stakingtypes.GetValidatorKey(valAddr))
		kvStore.Delete(stakingtypes.GetValidatorByConsAddrKey(valConsAddr))
		kvStore.Delete(stakingtypes.GetValidatorsByPowerIndexKey(v, wasmApp.StakingKeeper.PowerReduction(ctx), wasmApp.StakingKeeper.ValidatorAddressCodec()))
		kvStore.Delete(stakingtypes.GetLastValidatorPowerKey(valAddr))
	}

	// Set new validator
	if err := wasmApp.StakingKeeper.SetValidator(ctx, newVal); err != nil {
		return fmt.Errorf("failed to set validator: %w", err)
	}
	if err := wasmApp.StakingKeeper.SetValidatorByConsAddr(ctx, newVal); err != nil {
		return fmt.Errorf("failed to set validator by cons addr: %w", err)
	}
	if err := wasmApp.StakingKeeper.SetValidatorByPowerIndex(ctx, newVal); err != nil {
		return fmt.Errorf("failed to set validator power index: %w", err)
	}
	if err := wasmApp.StakingKeeper.SetLastValidatorPower(ctx, newValAddr, valVotingPower); err != nil {
		return fmt.Errorf("failed to set last validator power: %w", err)
	}

	// DISTRIBUTION
	if err := wasmApp.DistrKeeper.SetValidatorHistoricalRewards(ctx, newValAddr, 0, distrtypes.NewValidatorHistoricalRewards(sdk.DecCoins{}, 1)); err != nil {
		return err
	}
	if err := wasmApp.DistrKeeper.SetValidatorCurrentRewards(ctx, newValAddr, distrtypes.NewValidatorCurrentRewards(sdk.DecCoins{}, 1)); err != nil {
		return err
	}
	if err := wasmApp.DistrKeeper.SetValidatorAccumulatedCommission(ctx, newValAddr, distrtypes.InitialValidatorAccumulatedCommission()); err != nil {
		return err
	}
	if err := wasmApp.DistrKeeper.SetValidatorOutstandingRewards(ctx, newValAddr, distrtypes.ValidatorOutstandingRewards{Rewards: sdk.DecCoins{}}); err != nil {
		return err
	}

	// SLASHING
	newConsAddr := sdk.ConsAddress(pubkey.Address().Bytes())
	if err := wasmApp.SlashingKeeper.SetValidatorSigningInfo(ctx, newConsAddr, slashingtypes.ValidatorSigningInfo{
		Address:     newConsAddr.String(),
		StartHeight: wasmApp.LastBlockHeight() - 1,
		Tombstoned:  false,
	}); err != nil {
		return err
	}

	// GOVERNANCE — shorten voting periods
	govParams, err := wasmApp.GovKeeper.Params.Get(ctx)
	if err != nil {
		return err
	}
	votingPeriod := 20 * time.Second
	expeditedVotingPeriod := 10 * time.Second
	govParams.VotingPeriod = &votingPeriod
	govParams.ExpeditedVotingPeriod = &expeditedVotingPeriod
	if err := wasmApp.GovKeeper.Params.Set(ctx, govParams); err != nil {
		return err
	}

	// WASM — set permissionless
	wasmParams := wasmApp.WasmKeeper.GetParams(ctx)
	wasmParams.CodeUploadAccess = wasmtypes.AllowEverybody
	wasmParams.InstantiateDefaultPermission = wasmtypes.AccessTypeEverybody
	if err := wasmApp.WasmKeeper.SetParams(ctx, wasmParams); err != nil {
		return err
	}

	// COSMWASM ADMIN OVERRIDE
	if args.cosmwasmAdmin != "" {
		newAdminAddr, err := sdk.AccAddressFromBech32(args.cosmwasmAdmin)
		if err != nil {
			return fmt.Errorf("invalid cosmwasm admin address %q: %w", args.cosmwasmAdmin, err)
		}
		govKeeper := wasmkeeper.NewGovPermissionKeeper(&wasmApp.WasmKeeper)
		dummyCaller := sdk.AccAddress{}
		var count int
		wasmApp.WasmKeeper.IterateContractInfo(ctx, func(addr sdk.AccAddress, _ wasmtypes.ContractInfo) bool {
			if err = govKeeper.UpdateContractAdmin(ctx, addr, dummyCaller, newAdminAddr); err != nil {
				return true
			}
			count++
			return false
		})
		if err != nil {
			return fmt.Errorf("failed to update contract admin: %w", err)
		}
		fmt.Printf("Updated cosmwasm admin to %s on %d contracts\n", args.cosmwasmAdmin, count)
	}

	// UPGRADE
	if args.upgradeHeight > 0 {
		absoluteHeight := wasmApp.LastBlockHeight() + args.upgradeHeight
		if err := wasmApp.UpgradeKeeper.ScheduleUpgrade(ctx, upgradetypes.Plan{
			Name:   args.upgradeName,
			Height: absoluteHeight,
		}); err != nil {
			return err
		}
		fmt.Printf("Scheduled upgrade %q at height %d\n", args.upgradeName, absoluteHeight)
	}

	// BANK — fund accounts
	bondDenom, err := wasmApp.StakingKeeper.BondDenom(ctx)
	if err != nil {
		return err
	}
	var fundCoins sdk.Coins
	if args.coinsToFund != nil {
		fundCoins = args.coinsToFund
	} else {
		fundCoins = sdk.NewCoins(sdk.NewInt64Coin(bondDenom, 1_000_000_000_000_000))
	}
	for _, account := range args.accountsToFund {
		if err := wasmApp.BankKeeper.MintCoins(ctx, minttypes.ModuleName, fundCoins); err != nil {
			return fmt.Errorf("failed to mint coins for %s: %w", account, err)
		}
		if err := wasmApp.BankKeeper.SendCoinsFromModuleToAccount(ctx, minttypes.ModuleName, account, fundCoins); err != nil {
			return fmt.Errorf("failed to send coins to %s: %w", account, err)
		}
	}

	return nil
}

func saveValidatorsInfo(db cometdbm.DB, height int64, valInfo *cmtstate.ValidatorsInfo) error {
	bz, err := valInfo.Marshal()
	if err != nil {
		return err
	}
	return db.Set([]byte(fmt.Sprintf("validatorsKey:%v", height)), bz)
}

func openDB(rootDir, dbName string, backendType cometdbm.BackendType) (cometdbm.DB, error) {
	dataDir := filepath.Join(rootDir, "data")
	return cometdbm.NewDB(dbName, backendType, dataDir)
}
