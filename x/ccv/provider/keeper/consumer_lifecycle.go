package keeper

import (
	"fmt"
	"time"

	clienttypes "github.com/cosmos/ibc-go/v8/modules/core/02-client/types"
	channeltypes "github.com/cosmos/ibc-go/v8/modules/core/04-channel/types"
	commitmenttypes "github.com/cosmos/ibc-go/v8/modules/core/23-commitment/types"
	ibctmtypes "github.com/cosmos/ibc-go/v8/modules/light-clients/07-tendermint"

	errorsmod "cosmossdk.io/errors"
	storetypes "cosmossdk.io/store/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	tmtypes "github.com/cometbft/cometbft/types"

	"github.com/cosmos/interchain-security/v6/x/ccv/provider/types"
	ccv "github.com/cosmos/interchain-security/v6/x/ccv/types"
)

// PrepareConsumerForLaunch prepares to move the launch of a consumer chain from the previous spawn time to spawn time.
// Previous spawn time can correspond to its zero value if the validator was not previously set for launch.
func (k Keeper) PrepareConsumerForLaunch(ctx sdk.Context, consumerId string, previousSpawnTime, spawnTime time.Time) error {
	if !previousSpawnTime.IsZero() {
		// if this is not the first initialization and hence `previousSpawnTime` does not contain the zero value of `Time`
		// remove the consumer id from the previous spawn time
		err := k.RemoveConsumerToBeLaunched(ctx, consumerId, previousSpawnTime)
		if err != nil {
			return err
		}
	}
	return k.AppendConsumerToBeLaunched(ctx, consumerId, spawnTime)
}

// InitializeConsumer tries to move a consumer with `consumerId` to the initialized phase.
// If successful, it returns the spawn time and true.
func (k Keeper) InitializeConsumer(ctx sdk.Context, consumerId string) (time.Time, bool) {
	// a chain needs to be in the registered or initialized phase
	phase := k.GetConsumerPhase(ctx, consumerId)
	if phase != types.CONSUMER_PHASE_REGISTERED && phase != types.CONSUMER_PHASE_INITIALIZED {
		return time.Time{}, false
	}

	initializationParameters, err := k.GetConsumerInitializationParameters(ctx, consumerId)
	if err != nil {
		return time.Time{}, false
	}

	// the spawn time needs to be positive
	if initializationParameters.SpawnTime.IsZero() {
		return time.Time{}, false
	}

	k.SetConsumerPhase(ctx, consumerId, types.CONSUMER_PHASE_INITIALIZED)

	return initializationParameters.SpawnTime, true
}

// BeginBlockLaunchConsumers launches initialized consumers chains for which the spawn time has passed
func (k Keeper) BeginBlockLaunchConsumers(ctx sdk.Context) error {
	consumerIds, err := k.ConsumeIdsFromTimeQueue(
		ctx,
		types.SpawnTimeToConsumerIdsKeyPrefix(),
		k.GetConsumersToBeLaunched,
		k.DeleteAllConsumersToBeLaunched,
		k.AppendConsumerToBeLaunched,
		200,
	)
	if err != nil {
		return errorsmod.Wrapf(ccv.ErrInvalidConsumerState, "getting consumers ready to laumch: %s", err.Error())
	}
	for _, consumerId := range consumerIds {
		cachedCtx, writeFn := ctx.CacheContext()
		err = k.LaunchConsumer(cachedCtx, consumerId)
		if err != nil {
			ctx.Logger().Error("could not launch chain",
				"consumerId", consumerId,
				"error", err)
			continue
		}

		// the cached context is created with a new EventManager, so we merge the events into the original context
		ctx.EventManager().EmitEvents(cachedCtx.EventManager().Events())
		writeFn()
	}
	return nil
}

// ConsumeIdsFromTimeQueue returns from a time queue the consumer ids for which the associated time passed.
// The number of ids return is limited to 'limit'. The ids returned are removed from the time queue.
func (k Keeper) ConsumeIdsFromTimeQueue(
	ctx sdk.Context,
	timeQueueKeyPrefix byte,
	getIds func(sdk.Context, time.Time) (types.ConsumerIds, error),
	deleteAllIds func(sdk.Context, time.Time),
	appendId func(sdk.Context, string, time.Time) error,
	limit int,
) ([]string, error) {
	store := ctx.KVStore(k.storeKey)

	result := []string{}
	nextTime := []string{}
	timestampsToDelete := []time.Time{}

	iterator := storetypes.KVStorePrefixIterator(store, []byte{timeQueueKeyPrefix})
	defer iterator.Close()
	for ; iterator.Valid(); iterator.Next() {
		if len(result) >= limit {
			break
		}
		ts, err := types.ParseTime(timeQueueKeyPrefix, iterator.Key())
		if err != nil {
			return result, fmt.Errorf("parsing removal time: %w", err)
		}
		if ts.After(ctx.BlockTime()) {
			break
		}

		consumerIds, err := getIds(ctx, ts)
		if err != nil {
			return result,
				fmt.Errorf("getting consumers ids, ts(%s): %w", ts.String(), err)
		}

		timestampsToDelete = append(timestampsToDelete, ts)

		availableSlots := limit - len(result)
		if availableSlots >= len(consumerIds.Ids) {
			// consumer all the ids
			result = append(result, consumerIds.Ids...)
		} else {
			// consume only availableSlots
			result = append(result, consumerIds.Ids[:availableSlots]...)
			// and leave the others for next time
			nextTime = consumerIds.Ids[availableSlots:]
			break
		}
	}

	// remove consumers to prevent handling them twice
	for i, ts := range timestampsToDelete {
		deleteAllIds(ctx, ts)
		if i == len(timestampsToDelete)-1 {
			// for the last ts consumed, store back the ids for later
			for _, consumerId := range nextTime {
				err := appendId(ctx, consumerId, ts)
				if err != nil {
					return result,
						fmt.Errorf("failed to append consumer id, consumerId(%s), ts(%s): %w",
							consumerId, ts.String(), err)
				}
			}
		}
	}

	return result, nil
}

// LaunchConsumer launches the chain with the provided consumer id by creating the consumer client and the respective
// consumer genesis file
func (k Keeper) LaunchConsumer(ctx sdk.Context, consumerId string) error {
	err := k.CreateConsumerClient(ctx, consumerId)
	if err != nil {
		return err
	}

	consumerGenesis, found := k.GetConsumerGenesis(ctx, consumerId)
	if !found {
		return errorsmod.Wrapf(types.ErrNoConsumerGenesis, "consumer genesis could not be found for consumer id: %s", consumerId)
	}

	if len(consumerGenesis.Provider.InitialValSet) == 0 {
		return errorsmod.Wrapf(types.ErrInvalidConsumerGenesis, "consumer genesis initial validator set is empty - no validators opted in consumer id: %s", consumerId)
	}

	k.SetConsumerPhase(ctx, consumerId, types.CONSUMER_PHASE_LAUNCHED)

	return nil
}

// CreateConsumerClient will create the CCV client for the given consumer chain. The CCV channel must be built
// on top of the CCV client to ensure connection with the right consumer chain.
func (k Keeper) CreateConsumerClient(ctx sdk.Context, consumerId string) error {
	initializationRecord, err := k.GetConsumerInitializationParameters(ctx, consumerId)
	if err != nil {
		return err
	}

	phase := k.GetConsumerPhase(ctx, consumerId)
	if phase != types.CONSUMER_PHASE_INITIALIZED {
		return errorsmod.Wrapf(types.ErrInvalidPhase,
			"cannot create client for consumer chain that is not in the Initialized phase but in phase %d: %s", phase, consumerId)
	}

	chainId, err := k.GetConsumerChainId(ctx, consumerId)
	if err != nil {
		return err
	}

	// Set minimum height for equivocation evidence from this consumer chain
	k.SetEquivocationEvidenceMinHeight(ctx, consumerId, initializationRecord.InitialHeight.RevisionHeight)

	// Consumers start out with the unbonding period from the initialization parameters
	consumerUnbondingPeriod := initializationRecord.UnbondingPeriod

	// Create client state by getting template client from initialization parameters
	clientState := k.GetTemplateClient(ctx)
	clientState.ChainId = chainId
	clientState.LatestHeight = initializationRecord.InitialHeight

	trustPeriod, err := ccv.CalculateTrustPeriod(consumerUnbondingPeriod, k.GetTrustingPeriodFraction(ctx))
	if err != nil {
		return err
	}
	clientState.TrustingPeriod = trustPeriod
	clientState.UnbondingPeriod = consumerUnbondingPeriod

	consumerGen, validatorSetHash, err := k.MakeConsumerGenesis(ctx, consumerId)
	if err != nil {
		return err
	}
	err = k.SetConsumerGenesis(ctx, consumerId, consumerGen)
	if err != nil {
		return err
	}

	// Create consensus state
	consensusState := ibctmtypes.NewConsensusState(
		ctx.BlockTime(),
		commitmenttypes.NewMerkleRoot([]byte(ibctmtypes.SentinelRoot)),
		validatorSetHash, // use the hash of the updated initial valset
	)

	clientID, err := k.clientKeeper.CreateClient(ctx, clientState, consensusState)
	if err != nil {
		return err
	}
	k.SetConsumerClientId(ctx, consumerId, clientID)

	k.Logger(ctx).Info("consumer chain launched (client created)",
		"consumer id", consumerId,
		"client id", clientID,
	)

	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			types.EventTypeConsumerClientCreated,
			sdk.NewAttribute(sdk.AttributeKeyModule, types.ModuleName),
			sdk.NewAttribute(types.AttributeConsumerId, consumerId),
			sdk.NewAttribute(types.AttributeConsumerChainId, chainId),
			sdk.NewAttribute(clienttypes.AttributeKeyClientID, clientID),
			sdk.NewAttribute(types.AttributeInitialHeight, initializationRecord.InitialHeight.String()),
			sdk.NewAttribute(types.AttributeTrustingPeriod, clientState.TrustingPeriod.String()),
			sdk.NewAttribute(types.AttributeUnbondingPeriod, clientState.UnbondingPeriod.String()),
		),
	)

	return nil
}

// MakeConsumerGenesis returns the created consumer genesis state for consumer chain `consumerId`,
// as well as the validator hash of the initial validator set of the consumer chain
func (k Keeper) MakeConsumerGenesis(
	ctx sdk.Context,
	consumerId string,
) (gen ccv.ConsumerGenesisState, nextValidatorsHash []byte, err error) {
	initializationRecord, err := k.GetConsumerInitializationParameters(ctx, consumerId)
	if err != nil {
		return gen, nil, errorsmod.Wrapf(ccv.ErrInvalidConsumerState,
			"cannot retrieve initialization parameters: %s", err.Error())
	}
	powerShapingParameters, err := k.GetConsumerPowerShapingParameters(ctx, consumerId)
	if err != nil {
		return gen, nil, errorsmod.Wrapf(ccv.ErrInvalidConsumerState,
			"cannot retrieve power shaping parameters: %s", err.Error())
	}

	providerUnbondingPeriod, err := k.stakingKeeper.UnbondingTime(ctx)
	if err != nil {
		return gen, nil, errorsmod.Wrapf(types.ErrNoUnbondingTime, "unbonding time not found: %s", err)
	}
	height := clienttypes.GetSelfHeight(ctx)

	clientState := k.GetTemplateClient(ctx)
	// this is the counter party chain ID for the consumer
	clientState.ChainId = ctx.ChainID()
	// this is the latest height the client was updated at, i.e.,
	// the height of the latest consensus state (see below)
	clientState.LatestHeight = height
	trustPeriod, err := ccv.CalculateTrustPeriod(providerUnbondingPeriod, k.GetTrustingPeriodFraction(ctx))
	if err != nil {
		return gen, nil, errorsmod.Wrapf(sdkerrors.ErrInvalidHeight, "error %s calculating trusting_period for: %s", err, height)
	}
	clientState.TrustingPeriod = trustPeriod
	clientState.UnbondingPeriod = providerUnbondingPeriod

	consState, err := k.clientKeeper.GetSelfConsensusState(ctx, height)
	if err != nil {
		return gen, nil, errorsmod.Wrapf(clienttypes.ErrConsensusStateNotFound, "error %s getting self consensus state for: %s", err, height)
	}

	// get the bonded validators from the staking module
	bondedValidators, err := k.GetLastBondedValidators(ctx)
	if err != nil {
		return gen, nil, errorsmod.Wrapf(stakingtypes.ErrNoValidatorFound, "error getting last bonded validators: %s", err)
	}

	minPower := int64(0)
	if powerShapingParameters.Top_N > 0 {
		// get the consensus active validators
		// we do not want to base the power calculation for the top N
		// on inactive validators, too, since the top N will be a percentage of the active set power
		// otherwise, it could be that inactive validators are forced to validate
		activeValidators, err := k.GetLastProviderConsensusActiveValidators(ctx)
		if err != nil {
			return gen, nil, errorsmod.Wrapf(stakingtypes.ErrNoValidatorFound, "error getting last active bonded validators: %s", err)
		}

		// in a Top-N chain, we automatically opt in all validators that belong to the top N
		minPower, err = k.ComputeMinPowerInTopN(ctx, activeValidators, powerShapingParameters.Top_N)
		if err != nil {
			return gen, nil, err
		}
		// log the minimum power in top N
		k.Logger(ctx).Info("minimum power in top N at consumer genesis",
			"consumerId", consumerId,
			"minPower", minPower,
		)
		k.OptInTopNValidators(ctx, consumerId, activeValidators, minPower)
		k.SetMinimumPowerInTopN(ctx, consumerId, minPower)
	}

	// need to use the bondedValidators, not activeValidators, here since the chain might be opt-in and allow inactive vals
	nextValidators := k.ComputeNextValidators(ctx, consumerId, bondedValidators, powerShapingParameters, minPower)
	err = k.SetConsumerValSet(ctx, consumerId, nextValidators)
	if err != nil {
		return gen, nil, fmt.Errorf("unable to set consumer validator set in MakeConsumerGenesis: %s", err)
	}

	// get the initial updates with the latest set consumer public keys
	initialUpdatesWithConsumerKeys := DiffValidators([]types.ConsensusValidator{}, nextValidators)

	// Get a hash of the consumer validator set from the update with applied consumer assigned keys
	updatesAsValSet, err := tmtypes.PB2TM.ValidatorUpdates(initialUpdatesWithConsumerKeys)
	if err != nil {
		return gen, nil, fmt.Errorf("unable to create validator set from updates computed from key assignment in MakeConsumerGenesis: %s", err)
	}
	hash := tmtypes.NewValidatorSet(updatesAsValSet).Hash()

	// note that providerFeePoolAddrStr is sent to the consumer during the IBC Channel handshake;
	// see HandshakeMetadata in OnChanOpenTry on the provider-side, and OnChanOpenAck on the consumer-side
	consumerGenesisParams := ccv.NewParams(
		true,
		initializationRecord.BlocksPerDistributionTransmission,
		initializationRecord.DistributionTransmissionChannel,
		"", // providerFeePoolAddrStr,
		initializationRecord.CcvTimeoutPeriod,
		initializationRecord.TransferTimeoutPeriod,
		initializationRecord.ConsumerRedistributionFraction,
		initializationRecord.HistoricalEntries,
		initializationRecord.UnbondingPeriod,
		[]string{},
		[]string{},
		ccv.DefaultRetryDelayPeriod,
	)

	gen = *ccv.NewInitialConsumerGenesisState(
		clientState,
		consState.(*ibctmtypes.ConsensusState),
		initialUpdatesWithConsumerKeys,
		consumerGenesisParams,
	)
	return gen, hash, nil
}

// StopAndPrepareForConsumerRemoval sets the phase of the chain to stopped and prepares to get the state of the
// chain removed after unbonding period elapses
func (k Keeper) StopAndPrepareForConsumerRemoval(ctx sdk.Context, consumerId string) error {
	// The phase of the chain is immediately set to stopped, albeit its state is removed later (see below).
	// Setting the phase here helps in not considering this chain when we look at launched chains (e.g., in `QueueVSCPackets)
	k.SetConsumerPhase(ctx, consumerId, types.CONSUMER_PHASE_STOPPED)

	// state of this chain is removed once UnbondingPeriod elapses
	unbondingPeriod, err := k.stakingKeeper.UnbondingTime(ctx)
	if err != nil {
		return err
	}
	removalTime := ctx.BlockTime().Add(unbondingPeriod)

	if err := k.SetConsumerRemovalTime(ctx, consumerId, removalTime); err != nil {
		return fmt.Errorf("cannot set removal time (%s): %s", removalTime.String(), err.Error())
	}
	if err := k.AppendConsumerToBeRemoved(ctx, consumerId, removalTime); err != nil {
		return errorsmod.Wrapf(ccv.ErrInvalidConsumerState, "cannot set consumer to be removed: %s", err.Error())
	}

	return nil
}

// BeginBlockRemoveConsumers removes stopped consumer chain for which the removal time has passed
func (k Keeper) BeginBlockRemoveConsumers(ctx sdk.Context) error {
	consumerIds, err := k.ConsumeIdsFromTimeQueue(
		ctx,
		types.RemovalTimeToConsumerIdsKeyPrefix(),
		k.GetConsumersToBeRemoved,
		k.DeleteAllConsumersToBeRemoved,
		k.AppendConsumerToBeRemoved,
		200,
	)
	if err != nil {
		return errorsmod.Wrapf(ccv.ErrInvalidConsumerState, "getting consumers ready to stop: %s", err.Error())
	}
	for _, consumerId := range consumerIds {
		// delete consumer chain in a cached context to abort deletion in case of errors
		cachedCtx, writeFn := ctx.CacheContext()
		err = k.DeleteConsumerChain(cachedCtx, consumerId)
		if err != nil {
			k.Logger(ctx).Error("consumer chain could not be removed",
				"consumerId", consumerId,
				"error", err.Error())
			continue
		}

		// the cached context is created with a new EventManager so we merge the event into the original context
		ctx.EventManager().EmitEvents(cachedCtx.EventManager().Events())
		writeFn()
	}
	return nil
}

// DeleteConsumerChain cleans up the state of the given consumer chain
func (k Keeper) DeleteConsumerChain(ctx sdk.Context, consumerId string) (err error) {
	phase := k.GetConsumerPhase(ctx, consumerId)
	if phase != types.CONSUMER_PHASE_STOPPED {
		return fmt.Errorf("cannot delete non-stopped chain: %s", consumerId)
	}

	// clean up states
	k.DeleteConsumerClientId(ctx, consumerId)
	k.DeleteConsumerGenesis(ctx, consumerId)
	// Note: this call panics if the key assignment state is invalid
	k.DeleteKeyAssignments(ctx, consumerId)
	k.DeleteMinimumPowerInTopN(ctx, consumerId)
	k.DeleteEquivocationEvidenceMinHeight(ctx, consumerId)

	// close channel and delete the mappings between chain ID and channel ID
	if channelID, found := k.GetConsumerIdToChannelId(ctx, consumerId); found {
		// Close the channel for the given channel ID on the condition
		// that the channel exists and isn't already in the CLOSED state
		channel, found := k.channelKeeper.GetChannel(ctx, ccv.ProviderPortID, channelID)
		if found && channel.State != channeltypes.CLOSED {
			err := k.chanCloseInit(ctx, channelID)
			if err != nil {
				k.Logger(ctx).Error("channel to consumer chain could not be closed",
					"consumerId", consumerId,
					"channelID", channelID,
					"error", err.Error(),
				)
			}
		}
		k.DeleteConsumerIdToChannelId(ctx, consumerId)
		k.DeleteChannelIdToConsumerId(ctx, channelID)
	}

	// delete consumer commission rate
	provAddrs := k.GetAllCommissionRateValidators(ctx, consumerId)
	for _, addr := range provAddrs {
		k.DeleteConsumerCommissionRate(ctx, consumerId, addr)
	}

	k.DeleteInitChainHeight(ctx, consumerId)
	k.DeleteSlashAcks(ctx, consumerId)
	k.DeletePendingVSCPackets(ctx, consumerId)

	k.DeleteAllowlist(ctx, consumerId)
	k.DeleteDenylist(ctx, consumerId)
	k.DeleteAllOptedIn(ctx, consumerId)
	k.DeleteConsumerValSet(ctx, consumerId)

	k.DeleteConsumerRewardsAllocation(ctx, consumerId)
	k.DeleteConsumerRemovalTime(ctx, consumerId)

	// TODO (PERMISSIONLESS) add newly-added state to be deleted

	// Note that we do not delete ConsumerIdToChainIdKey and ConsumerIdToPhase, as well
	// as consumer metadata, initialization and power-shaping parameters.
	// This is to enable block explorers and front ends to show information of
	// consumer chains that were removed without needing an archive node.

	k.SetConsumerPhase(ctx, consumerId, types.CONSUMER_PHASE_DELETED)
	k.Logger(ctx).Info("consumer chain deleted from provider", "consumerId", consumerId)

	return nil
}

//
// Setters and Getters
//

// GetConsumerRemovalTime returns the removal time associated with the to-be-removed chain with consumer id
func (k Keeper) GetConsumerRemovalTime(ctx sdk.Context, consumerId string) (time.Time, error) {
	store := ctx.KVStore(k.storeKey)
	buf := store.Get(types.ConsumerIdToRemovalTimeKey(consumerId))
	if buf == nil {
		return time.Time{}, fmt.Errorf("failed to retrieve removal time for consumer id (%s)", consumerId)
	}
	var time time.Time
	if err := time.UnmarshalBinary(buf); err != nil {
		return time, fmt.Errorf("failed to unmarshal removal time for consumer id (%s): %w", consumerId, err)
	}
	return time, nil
}

// SetConsumerRemovalTime sets the removal time associated with this consumer id
func (k Keeper) SetConsumerRemovalTime(ctx sdk.Context, consumerId string, removalTime time.Time) error {
	store := ctx.KVStore(k.storeKey)
	buf, err := removalTime.MarshalBinary()
	if err != nil {
		return fmt.Errorf("failed to marshal removal time (%+v) for consumer id (%s): %w", removalTime, consumerId, err)
	}
	store.Set(types.ConsumerIdToRemovalTimeKey(consumerId), buf)
	return nil
}

// DeleteConsumerRemovalTime deletes the removal time associated with this consumer id
func (k Keeper) DeleteConsumerRemovalTime(ctx sdk.Context, consumerId string) {
	store := ctx.KVStore(k.storeKey)
	store.Delete(types.ConsumerIdToRemovalTimeKey(consumerId))
}

// getConsumerIdsBasedOnTime returns all the consumer ids stored under this specific `key(time)`
func (k Keeper) getConsumerIdsBasedOnTime(ctx sdk.Context, key func(time.Time) []byte, time time.Time) (types.ConsumerIds, error) {
	store := ctx.KVStore(k.storeKey)
	bz := store.Get(key(time))
	if bz == nil {
		return types.ConsumerIds{}, nil
	}

	var consumerIds types.ConsumerIds

	if err := consumerIds.Unmarshal(bz); err != nil {
		return types.ConsumerIds{}, fmt.Errorf("failed to unmarshal consumer ids: %w", err)
	}
	return consumerIds, nil
}

// appendConsumerIdOnTime appends the consumer id on all the other consumer ids under `key(time)`
func (k Keeper) appendConsumerIdOnTime(ctx sdk.Context, consumerId string, key func(time.Time) []byte, time time.Time) error {
	store := ctx.KVStore(k.storeKey)

	consumers, err := k.getConsumerIdsBasedOnTime(ctx, key, time)
	if err != nil {
		return err
	}

	consumersWithAppend := types.ConsumerIds{
		Ids: append(consumers.Ids, consumerId),
	}

	bz, err := consumersWithAppend.Marshal()
	if err != nil {
		return err
	}

	store.Set(key(time), bz)
	return nil
}

// removeConsumerIdFromTime removes consumer id stored under `key(time)`
func (k Keeper) removeConsumerIdFromTime(ctx sdk.Context, consumerId string, key func(time.Time) []byte, time time.Time) error {
	store := ctx.KVStore(k.storeKey)

	consumers, err := k.getConsumerIdsBasedOnTime(ctx, key, time)
	if err != nil {
		return err
	}

	if len(consumers.Ids) == 0 {
		return fmt.Errorf("no consumer ids found for this time: %s", time.String())
	}

	// find the index of the consumer we want to remove
	index := -1
	for i := 0; i < len(consumers.Ids); i++ {
		if consumers.Ids[i] == consumerId {
			index = i
			break
		}
	}

	if index == -1 {
		return fmt.Errorf("failed to find consumer id (%s)", consumerId)
	}

	if len(consumers.Ids) == 1 {
		store.Delete(key(time))
		return nil
	}

	consumersWithRemoval := types.ConsumerIds{
		Ids: append(consumers.Ids[:index], consumers.Ids[index+1:]...),
	}

	bz, err := consumersWithRemoval.Marshal()
	if err != nil {
		return err
	}

	store.Set(key(time), bz)
	return nil
}

// GetConsumersToBeLaunched returns all the consumer ids of chains stored under this spawn time
func (k Keeper) GetConsumersToBeLaunched(ctx sdk.Context, spawnTime time.Time) (types.ConsumerIds, error) {
	return k.getConsumerIdsBasedOnTime(ctx, types.SpawnTimeToConsumerIdsKey, spawnTime)
}

// AppendConsumerToBeLaunched appends the provider consumer id for the given spawn time
func (k Keeper) AppendConsumerToBeLaunched(ctx sdk.Context, consumerId string, spawnTime time.Time) error {
	return k.appendConsumerIdOnTime(ctx, consumerId, types.SpawnTimeToConsumerIdsKey, spawnTime)
}

// RemoveConsumerToBeLaunched removes consumer id from if stored for this specific spawn time
func (k Keeper) RemoveConsumerToBeLaunched(ctx sdk.Context, consumerId string, spawnTime time.Time) error {
	return k.removeConsumerIdFromTime(ctx, consumerId, types.SpawnTimeToConsumerIdsKey, spawnTime)
}

// DeleteAllConsumersToBeLaunched deletes all consumer to be launched at this specific spawn time
func (k Keeper) DeleteAllConsumersToBeLaunched(ctx sdk.Context, spawnTime time.Time) {
	store := ctx.KVStore(k.storeKey)
	store.Delete(types.SpawnTimeToConsumerIdsKey(spawnTime))
}

// GetConsumersToBeRemoved returns all the consumer ids of chains stored under this removal time
func (k Keeper) GetConsumersToBeRemoved(ctx sdk.Context, removalTime time.Time) (types.ConsumerIds, error) {
	return k.getConsumerIdsBasedOnTime(ctx, types.RemovalTimeToConsumerIdsKey, removalTime)
}

// AppendConsumerToBeRemoved appends the provider consumer id for the given removal time
func (k Keeper) AppendConsumerToBeRemoved(ctx sdk.Context, consumerId string, removalTime time.Time) error {
	return k.appendConsumerIdOnTime(ctx, consumerId, types.RemovalTimeToConsumerIdsKey, removalTime)
}

// RemoveConsumerToBeRemoved removes consumer id from the given removal time
func (k Keeper) RemoveConsumerToBeRemoved(ctx sdk.Context, consumerId string, removalTime time.Time) error {
	return k.removeConsumerIdFromTime(ctx, consumerId, types.RemovalTimeToConsumerIdsKey, removalTime)
}

// DeleteAllConsumersToBeRemoved deletes all consumer to be removed at this specific removal time
func (k Keeper) DeleteAllConsumersToBeRemoved(ctx sdk.Context, removalTime time.Time) {
	store := ctx.KVStore(k.storeKey)
	store.Delete(types.RemovalTimeToConsumerIdsKey(removalTime))
}
