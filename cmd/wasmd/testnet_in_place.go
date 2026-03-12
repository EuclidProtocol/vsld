package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	cmtcrypto "github.com/cometbft/cometbft/crypto"
	tmproto "github.com/cometbft/cometbft/proto/tendermint/types"
	dbm "github.com/cosmos/cosmos-db"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"cosmossdk.io/log"
	"cosmossdk.io/math"
	upgradetypes "cosmossdk.io/x/upgrade/types"

	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	"github.com/cosmos/cosmos-sdk/server"
	servertypes "github.com/cosmos/cosmos-sdk/server/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	distrtypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	minttypes "github.com/cosmos/cosmos-sdk/x/mint/types"
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	"github.com/CosmWasm/wasmd/app"
)

const (
	flagTestnetAccountsToFund = "accounts-to-fund"
	flagTestnetCoinsToFund    = "coins-to-fund"
	flagTestnetUpgradeHeight  = "upgrade-height"
	flagTestnetUpgradeName    = "upgrade-name"

	testnetValidatorPower = 900000000000000
)

// testnetArgs holds the parsed arguments for testnet state modification.
type testnetArgs struct {
	newOpAddr      string
	validatorAddr  cmtcrypto.Address
	userPubKey     cmtcrypto.PubKey
	accountsToFund string
	coinsToFund    string
	upgradeHeight  int64
	upgradeName    string
}

// InPlaceTestnetCmd returns the cobra command for creating an in-place testnet.
func InPlaceTestnetCmd() *cobra.Command {
	cmd := server.InPlaceTestnetCreator(newTestnetApp)
	cmd.Example = "in-place-testnet local-euclid euclidvaloper1abc... --accounts-to-fund=euclid1xyz... --skip-confirmation"
	cmd.Flags().String(flagTestnetAccountsToFund, "", "Comma-separated list of account addresses to fund")
	cmd.Flags().String(flagTestnetCoinsToFund, "", "Coins per funded account (e.g., 1000000000ulumen)")
	cmd.Flags().Int64(flagTestnetUpgradeHeight, 0, "Schedule upgrade at current height + this offset")
	cmd.Flags().String(flagTestnetUpgradeName, "", "Name of the upgrade to schedule")
	return cmd
}

// newTestnetApp wraps newApp to apply application-state modifications for in-place testnet.
func newTestnetApp(
	logger log.Logger,
	db dbm.DB,
	traceStore io.Writer,
	appOpts servertypes.AppOptions,
) servertypes.Application {
	baseApp := newApp(logger, db, traceStore, appOpts)

	wasmApp, ok := baseApp.(*app.WasmApp)
	if !ok {
		panic("app is not *app.WasmApp")
	}

	viperAppOpts, ok := appOpts.(*viper.Viper)
	if !ok {
		panic("appOpts is not *viper.Viper")
	}

	// SDK-provided testnet keys (set by testnetify before calling this creator)
	args := testnetArgs{
		newOpAddr:      viperAppOpts.GetString(server.KeyNewOpAddr),
		accountsToFund: viperAppOpts.GetString(flagTestnetAccountsToFund),
		coinsToFund:    viperAppOpts.GetString(flagTestnetCoinsToFund),
		upgradeHeight:  viperAppOpts.GetInt64(flagTestnetUpgradeHeight),
		upgradeName:    viperAppOpts.GetString(flagTestnetUpgradeName),
	}

	// These are set by the SDK's testnetify as raw Go types via Viper.Set
	if addr, ok := viperAppOpts.Get(server.KeyNewValAddr).(cmtcrypto.Address); ok {
		args.validatorAddr = addr
	}
	if pk, ok := viperAppOpts.Get(server.KeyUserPubKey).(cmtcrypto.PubKey); ok {
		args.userPubKey = pk
	}

	// Only modify app state if we have the necessary keys from testnetify
	if args.userPubKey != nil && len(args.validatorAddr) > 0 {
		if err := modifyAppState(wasmApp, args); err != nil {
			panic(fmt.Sprintf("failed to modify app state for testnet: %v", err))
		}
	}

	return wasmApp
}

// modifyAppState performs Gaia-style application-state modifications for the in-place testnet.
func modifyAppState(wasmApp *app.WasmApp, args testnetArgs) error {
	ctx := wasmApp.NewUncachedContext(true, tmproto.Header{})

	// Convert CometBFT pubkey to SDK pubkey
	sdkPubKey, err := cryptocodec.FromCmtPubKeyInterface(args.userPubKey)
	if err != nil {
		return fmt.Errorf("failed to convert pubkey: %w", err)
	}

	// Parse operator address
	newOpAddr, err := sdk.ValAddressFromBech32(args.newOpAddr)
	if err != nil {
		return fmt.Errorf("invalid operator address %q: %w", args.newOpAddr, err)
	}

	// Derive consensus and delegator addresses
	newConsAddr := sdk.ConsAddress(args.validatorAddr)
	delegatorAddr := sdk.AccAddress(newOpAddr.Bytes())

	// Get bond denom
	bondDenom, err := wasmApp.StakingKeeper.BondDenom(ctx)
	if err != nil {
		return fmt.Errorf("failed to get bond denom: %w", err)
	}

	if err := modifyStaking(ctx, wasmApp, newOpAddr, delegatorAddr, sdkPubKey); err != nil {
		return err
	}
	if err := modifyDistribution(ctx, wasmApp, newOpAddr); err != nil {
		return err
	}
	if err := modifySlashing(ctx, wasmApp, newConsAddr); err != nil {
		return err
	}
	if err := modifyGovernance(ctx, wasmApp); err != nil {
		return err
	}
	if args.accountsToFund != "" {
		if err := fundAccounts(ctx, wasmApp, args.accountsToFund, args.coinsToFund, bondDenom); err != nil {
			return err
		}
	}
	if args.upgradeHeight > 0 && args.upgradeName != "" {
		if err := scheduleUpgrade(ctx, wasmApp, args.upgradeName, args.upgradeHeight); err != nil {
			return err
		}
	}

	return nil
}

// modifyStaking removes old validator powers and creates the new testnet validator.
func modifyStaking(
	ctx sdk.Context,
	wasmApp *app.WasmApp,
	newOpAddr sdk.ValAddress,
	delegatorAddr sdk.AccAddress,
	sdkPubKey cryptotypes.PubKey,
) error {
	// Remove last validator power and power index for all existing validators
	allVals, err := wasmApp.StakingKeeper.GetAllValidators(ctx)
	if err != nil {
		return fmt.Errorf("failed to get validators: %w", err)
	}
	for _, val := range allVals {
		valAddr, err := sdk.ValAddressFromBech32(val.OperatorAddress)
		if err != nil {
			continue
		}
		if err := wasmApp.StakingKeeper.DeleteLastValidatorPower(ctx, valAddr); err != nil {
			return fmt.Errorf("failed to delete last validator power for %s: %w", val.OperatorAddress, err)
		}
		if err := wasmApp.StakingKeeper.DeleteValidatorByPowerIndex(ctx, val); err != nil {
			return fmt.Errorf("failed to delete validator power index for %s: %w", val.OperatorAddress, err)
		}
	}

	// Create new validator with full power
	validatorTokens := sdk.DefaultPowerReduction.MulRaw(testnetValidatorPower)

	newVal, err := stakingtypes.NewValidator(
		newOpAddr.String(),
		sdkPubKey,
		stakingtypes.Description{Moniker: "testnet-validator"},
	)
	if err != nil {
		return fmt.Errorf("failed to create validator: %w", err)
	}

	newVal.Status = stakingtypes.Bonded
	newVal.Tokens = validatorTokens
	newVal.DelegatorShares = math.LegacyNewDecFromInt(validatorTokens)

	if err := wasmApp.StakingKeeper.SetValidator(ctx, newVal); err != nil {
		return fmt.Errorf("failed to set validator: %w", err)
	}
	if err := wasmApp.StakingKeeper.SetValidatorByConsAddr(ctx, newVal); err != nil {
		return fmt.Errorf("failed to set validator by cons addr: %w", err)
	}
	if err := wasmApp.StakingKeeper.SetValidatorByPowerIndex(ctx, newVal); err != nil {
		return fmt.Errorf("failed to set validator power index: %w", err)
	}
	if err := wasmApp.StakingKeeper.SetLastValidatorPower(ctx, newOpAddr, testnetValidatorPower); err != nil {
		return fmt.Errorf("failed to set last validator power: %w", err)
	}

	// Create self-delegation
	delegation := stakingtypes.NewDelegation(
		delegatorAddr.String(),
		newOpAddr.String(),
		math.LegacyNewDecFromInt(validatorTokens),
	)
	if err := wasmApp.StakingKeeper.SetDelegation(ctx, delegation); err != nil {
		return fmt.Errorf("failed to set delegation: %w", err)
	}

	return nil
}

// modifyDistribution initializes distribution state for the new validator.
func modifyDistribution(ctx sdk.Context, wasmApp *app.WasmApp, newOpAddr sdk.ValAddress) error {
	if err := wasmApp.DistrKeeper.SetValidatorHistoricalRewards(ctx, newOpAddr, 0, distrtypes.NewValidatorHistoricalRewards(sdk.DecCoins{}, 1)); err != nil {
		return fmt.Errorf("failed to set historical rewards: %w", err)
	}
	if err := wasmApp.DistrKeeper.SetValidatorCurrentRewards(ctx, newOpAddr, distrtypes.NewValidatorCurrentRewards(sdk.DecCoins{}, 1)); err != nil {
		return fmt.Errorf("failed to set current rewards: %w", err)
	}
	if err := wasmApp.DistrKeeper.SetValidatorAccumulatedCommission(ctx, newOpAddr, distrtypes.ValidatorAccumulatedCommission{}); err != nil {
		return fmt.Errorf("failed to set accumulated commission: %w", err)
	}
	if err := wasmApp.DistrKeeper.SetValidatorOutstandingRewards(ctx, newOpAddr, distrtypes.ValidatorOutstandingRewards{Rewards: sdk.DecCoins{}}); err != nil {
		return fmt.Errorf("failed to set outstanding rewards: %w", err)
	}
	return nil
}

// modifySlashing sets signing info for the new validator.
func modifySlashing(ctx sdk.Context, wasmApp *app.WasmApp, newConsAddr sdk.ConsAddress) error {
	signingInfo := slashingtypes.NewValidatorSigningInfo(
		newConsAddr,
		0,                    // start height
		0,                    // index offset
		time.Unix(0, 0).UTC(), // jailed until
		false,                // tombstoned
		0,                    // missed blocks counter
	)
	if err := wasmApp.SlashingKeeper.SetValidatorSigningInfo(ctx, newConsAddr, signingInfo); err != nil {
		return fmt.Errorf("failed to set signing info: %w", err)
	}
	return nil
}

// modifyGovernance shortens voting periods for testnet convenience.
func modifyGovernance(ctx sdk.Context, wasmApp *app.WasmApp) error {
	govParams, err := wasmApp.GovKeeper.Params.Get(ctx)
	if err != nil {
		return fmt.Errorf("failed to get gov params: %w", err)
	}

	votingPeriod := 20 * time.Second
	expeditedVotingPeriod := 10 * time.Second
	govParams.VotingPeriod = &votingPeriod
	govParams.ExpeditedVotingPeriod = &expeditedVotingPeriod

	if err := wasmApp.GovKeeper.Params.Set(ctx, govParams); err != nil {
		return fmt.Errorf("failed to set gov params: %w", err)
	}
	return nil
}

// fundAccounts mints and sends coins to the specified accounts.
func fundAccounts(ctx sdk.Context, wasmApp *app.WasmApp, accountsToFund, coinsToFund, bondDenom string) error {
	coins := sdk.NewCoins(sdk.NewCoin(bondDenom, math.NewInt(1_000_000_000_000_000)))
	if coinsToFund != "" {
		var err error
		coins, err = sdk.ParseCoinsNormalized(coinsToFund)
		if err != nil {
			return fmt.Errorf("failed to parse --coins-to-fund: %w", err)
		}
	}

	for _, accStr := range strings.Split(accountsToFund, ",") {
		accStr = strings.TrimSpace(accStr)
		if accStr == "" {
			continue
		}

		accAddr, err := sdk.AccAddressFromBech32(accStr)
		if err != nil {
			return fmt.Errorf("invalid account address %q: %w", accStr, err)
		}

		if err := wasmApp.BankKeeper.MintCoins(ctx, minttypes.ModuleName, coins); err != nil {
			return fmt.Errorf("failed to mint coins for %s: %w", accStr, err)
		}
		if err := wasmApp.BankKeeper.SendCoinsFromModuleToAccount(ctx, minttypes.ModuleName, accAddr, coins); err != nil {
			return fmt.Errorf("failed to send coins to %s: %w", accStr, err)
		}
	}

	return nil
}

// scheduleUpgrade schedules a software upgrade at the given height offset.
func scheduleUpgrade(ctx sdk.Context, wasmApp *app.WasmApp, name string, heightOffset int64) error {
	plan := upgradetypes.Plan{
		Name:   name,
		Height: ctx.BlockHeight() + heightOffset,
	}
	if err := wasmApp.UpgradeKeeper.ScheduleUpgrade(ctx, plan); err != nil {
		return fmt.Errorf("failed to schedule upgrade %q: %w", name, err)
	}
	return nil
}
