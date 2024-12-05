package core

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/Layr-Labs/eigenpod-proofs-generation/cli/core/onchain"
	"github.com/attestantio/go-eth2-client/spec"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	gethCommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

type Checkpoint struct {
	ProofsRemaining uint64
	StartedAt       uint64
}

type Validator struct {
	Slashed                             bool
	Index                               uint64
	Status                              int
	PublicKey                           string
	IsAwaitingActivationQueue           bool
	IsAwaitingWithdrawalCredentialProof bool
	EffectiveBalance                    uint64
	CurrentBalance                      uint64
}

type EigenpodStatus struct {
	Validators map[string]Validator

	ActiveCheckpoint *Checkpoint

	NumberValidatorsToCheckpoint int

	CurrentTotalSharesETH *big.Float
	Status                int

	// if you completed a new checkpoint right now, how many shares would you get?
	//
	//  this is computed as:
	// 		- If checkpoint is already started:
	// 			sum(beacon chain balances) + currentCheckpoint.podBalanceGwei + pod.withdrawableRestakedExecutionLayerGwei()
	// 		- If no checkpoint is started:
	// 			total_shares_after_checkpoint = sum(validator[i].regular_balance) + (balanceOf(pod) rounded down to gwei) - withdrawableRestakedExecutionLayerGwei
	TotalSharesAfterCheckpointGwei *big.Float
	TotalSharesAfterCheckpointETH  *big.Float

	PodOwner       gethCommon.Address
	ProofSubmitter gethCommon.Address

	// Whether the checkpoint would need to be started with the `--force` flag.
	// This would be due to the pod not having any uncheckpointed native ETH
	MustForceCheckpoint bool
}

func getRegularBalancesGwei(state *spec.VersionedBeaconState) []phase0.Gwei {
	validatorBalances, err := state.ValidatorBalances()
	PanicOnError("failed to load validator balances", err)

	return validatorBalances
}

func sumValidatorBeaconBalancesGwei(allValidators []ValidatorWithOnchainInfo, allBalances []phase0.Gwei) *big.Int {
	sumGwei := big.NewInt(0)

	for i := 0; i < len(allValidators); i++ {
		validator := allValidators[i]
		sumGwei = sumGwei.Add(sumGwei, new(big.Int).SetUint64(uint64(allBalances[validator.Index])))
	}

	return sumGwei
}

func sfSumValidatorBeaconBalancesGwei(allValidators []SfValidatorWithOnchainInfo) *big.Int {
	sumGwei := big.NewInt(0)

	for i := 0; i < len(allValidators); i++ {
		validator := allValidators[i]
		sumGwei = sumGwei.Add(sumGwei, new(big.Int).SetUint64(uint64(validator.Validator.Balance)))
	}

	return sumGwei
}

func sumRestakedBalancesGwei(activeValidators []ValidatorWithOnchainInfo) *big.Int {
	sumGwei := big.NewInt(0)

	for i := 0; i < len(activeValidators); i++ {
		validator := activeValidators[i]
		sumGwei = sumGwei.Add(sumGwei, new(big.Int).SetUint64(validator.Info.RestakedBalanceGwei))
	}

	return sumGwei
}

func sfSumRestakedBalancesGwei(activeValidators []SfValidatorWithOnchainInfo) *big.Int {
	sumGwei := big.NewInt(0)

	for i := 0; i < len(activeValidators); i++ {
		validator := activeValidators[i]
		sumGwei = sumGwei.Add(sumGwei, new(big.Int).SetUint64(validator.Info.RestakedBalanceGwei))
	}

	return sumGwei
}

func GetStatus(ctx context.Context, eigenpodAddress string, eth *ethclient.Client, beaconClient BeaconClient) EigenpodStatus {
	validators := map[string]Validator{}
	var activeCheckpoint *Checkpoint = nil

	eigenPod, err := onchain.NewEigenPod(gethCommon.HexToAddress(eigenpodAddress), eth)
	PanicOnError("failed to reach eigenpod", err)

	checkpoint, err := eigenPod.CurrentCheckpoint(nil)
	PanicOnError("failed to fetch checkpoint information", err)

	// Fetch the beacon state associated with the checkpoint (or "head" if there is no checkpoint)
	checkpointTimestamp, state, err := GetCheckpointTimestampAndBeaconState(ctx, eigenpodAddress, eth, beaconClient)
	PanicOnError("failed to fetch checkpoint and beacon state", err)

	allValidatorsForEigenpod, err := FindAllValidatorsForEigenpod(eigenpodAddress, state)
	PanicOnError("failed to find validators", err)

	allValidatorsWithInfoForEigenpod, err := FetchMultipleOnchainValidatorInfo(ctx, eth, eigenpodAddress, allValidatorsForEigenpod)
	PanicOnError("failed to fetch validator info", err)

	allBeaconBalancesGwei := getRegularBalancesGwei(state)

	activeValidators, err := SelectActiveValidators(eth, eigenpodAddress, allValidatorsWithInfoForEigenpod)
	PanicOnError("failed to find active validators", err)

	checkpointableValidators, err := SelectCheckpointableValidators(eth, eigenpodAddress, allValidatorsWithInfoForEigenpod, checkpointTimestamp)
	PanicOnError("failed to find checkpointable validators", err)

	sumBeaconBalancesWei := IGweiToWei(sumValidatorBeaconBalancesGwei(activeValidators, allBeaconBalancesGwei))
	sumRestakedBalancesWei := IGweiToWei(sumRestakedBalancesGwei(activeValidators))

	PanicOnError("failed to calculate sum of onchain validator balances", err)

	for _, validator := range allValidatorsWithInfoForEigenpod {
		validators[fmt.Sprintf("%d", validator.Index)] = Validator{
			Index:                               validator.Index,
			Status:                              int(validator.Info.Status),
			Slashed:                             validator.Validator.Slashed,
			PublicKey:                           validator.Validator.PublicKey.String(),
			IsAwaitingActivationQueue:           validator.Validator.ActivationEpoch == FAR_FUTURE_EPOCH,
			IsAwaitingWithdrawalCredentialProof: IsAwaitingWithdrawalCredentialProof(validator.Info, validator.Validator),
			EffectiveBalance:                    uint64(validator.Validator.EffectiveBalance),
			CurrentBalance:                      uint64(allBeaconBalancesGwei[validator.Index]),
		}
	}

	eigenpodManagerContractAddress, err := eigenPod.EigenPodManager(nil)
	PanicOnError("failed to get manager address", err)

	eigenPodManager, err := onchain.NewEigenPodManager(eigenpodManagerContractAddress, eth)
	PanicOnError("failed to get manager instance", err)

	eigenPodOwner, err := eigenPod.PodOwner(nil)
	PanicOnError("failed to get eigenpod owner", err)

	proofSubmitter, err := eigenPod.ProofSubmitter(nil)
	PanicOnError("failed to get eigenpod proof submitter", err)

	currentOwnerShares, err := eigenPodManager.PodOwnerShares(nil, eigenPodOwner)
	// currentOwnerShares = big.NewInt(0)
	PanicOnError("failed to load pod owner shares", err)
	currentOwnerSharesETH := IweiToEther(currentOwnerShares)
	currentOwnerSharesWei := currentOwnerShares

	withdrawableRestakedExecutionLayerGwei, err := eigenPod.WithdrawableRestakedExecutionLayerGwei(nil)
	PanicOnError("failed to fetch withdrawableRestakedExecutionLayerGwei", err)

	// Estimate the total shares we'll have if we complete an existing checkpoint
	// (or start a new one and complete that).
	//
	// First, we need the change in the pod's native ETH balance since the last checkpoint:
	var nativeETHDeltaWei *big.Int
	mustForceCheckpoint := false

	if checkpointTimestamp != 0 {
		// Change in the pod's native ETH balance (already calculated for us when the checkpoint was started)
		fmt.Printf("pod had a checkpoint\n")
		nativeETHDeltaWei = IGweiToWei(new(big.Int).SetUint64(checkpoint.PodBalanceGwei))

		// Remove already-computed delta from an in-progress checkpoint
		sumRestakedBalancesWei = new(big.Int).Sub(
			sumRestakedBalancesWei,
			IGweiToWei(checkpoint.BalanceDeltasGwei),
		)

		activeCheckpoint = &Checkpoint{
			ProofsRemaining: checkpoint.ProofsRemaining.Uint64(),
			StartedAt:       checkpointTimestamp,
		}
	} else {
		fmt.Printf("pod did not have a checkpoint\n")
		latestPodBalanceWei, err := eth.BalanceAt(ctx, gethCommon.HexToAddress(eigenpodAddress), nil)
		PanicOnError("failed to fetch pod balance", err)

		// We don't have a checkpoint currently, so we need to calculate what
		// checkpoint.PodBalanceGwei would be if we started one now:
		nativeETHDeltaWei = new(big.Int).Sub(
			latestPodBalanceWei,
			IGweiToWei(new(big.Int).SetUint64(withdrawableRestakedExecutionLayerGwei)),
		)

		// Determine whether the checkpoint needs to be started with `--force`
		if nativeETHDeltaWei.Sign() == 0 {
			mustForceCheckpoint = true
		}
	}

	// Next, we need the change in the pod's beacon chain balances since the last
	// checkpoint:
	//
	// beaconETHDeltaWei = sumBeaconBalancesWei - sumRestakedBalancesWei
	beaconETHDeltaWei := new(big.Int).Sub(
		sumBeaconBalancesWei,
		sumRestakedBalancesWei,
	)

	// Sum of these two deltas represents the change in shares after this checkpoint
	totalShareDeltaWei := new(big.Int).Add(
		nativeETHDeltaWei,
		beaconETHDeltaWei,
	)

	// Calculate new total shares by applying delta to current shares
	pendingSharesWei := new(big.Int).Add(
		currentOwnerSharesWei,
		totalShareDeltaWei,
	)

	pendingEth := GweiToEther(WeiToGwei(pendingSharesWei))

	return EigenpodStatus{
		Validators:                     validators,
		ActiveCheckpoint:               activeCheckpoint,
		CurrentTotalSharesETH:          currentOwnerSharesETH,
		TotalSharesAfterCheckpointGwei: WeiToGwei(pendingSharesWei),
		TotalSharesAfterCheckpointETH:  pendingEth,
		NumberValidatorsToCheckpoint:   len(checkpointableValidators),
		PodOwner:                       eigenPodOwner,
		ProofSubmitter:                 proofSubmitter,
		MustForceCheckpoint:            mustForceCheckpoint,
	}
}
func GetStatus_SF(ctx context.Context, eigenpodAddress string, eth *ethclient.Client, beaconClient BeaconClient) EigenpodStatus {
	validators := map[string]Validator{}
	var activeCheckpoint *Checkpoint = nil

	startTime := time.Now()
	eigenPod, err := onchain.NewEigenPod(gethCommon.HexToAddress(eigenpodAddress), eth)
	PanicOnError("failed to reach eigenpod", err)
	fmt.Printf("Time taken to reach eigenpod: %v\n", time.Since(startTime))

	startTime = time.Now()
	checkpoint, err := eigenPod.CurrentCheckpoint(nil)
	PanicOnError("failed to fetch checkpoint information", err)
	fmt.Printf("Time taken to fetch checkpoint information: %v\n", time.Since(startTime))

	// Fetch the allValidatorsForEigenpod state associated with the checkpoint (or "head" if there is no checkpoint)
	startTime = time.Now()
	checkpointTimestamp, allValidatorsForEigenpod, err := SfGetCheckpointTimestampAndBeaconState(ctx, eigenpodAddress, []uint64{1442700,
		1442701,
		1442702,
		1442703,
		1442704,
		1442705,
		1442706,
		1442707,
		1442708,
		1442709,
		1442710,
		1442711,
		1442712,
		1442713,
		1442714,
		1442715,
		1442716,
		1442717,
		1442718,
		1442719,
		1442720,
		1442721,
		1442722,
		1442723,
		1442724,
		1442725,
		1442726,
		1442727,
		1442728,
		1442729,
		1442730,
		1442731,
		1442732,
		1442733,
		1442734,
		1442735,
		1442736,
		1442737,
		1442738,
		1442739,
		1442740,
		1442741,
		1442742,
		1442743,
		1442744,
		1442745,
		1442746,
		1442747,
		1442748,
		1442749,
		1442750,
		1442751,
		1442752,
		1442753,
		1442754,
		1442755,
		1442756,
		1442757,
		1442758,
		1442759,
		1442760,
		1442761,
		1442762,
		1442763,
		1442764,
		1442765,
		1442766,
		1442767,
		1442768,
		1442769,
		1442770,
		1442771,
		1442772,
		1442773,
		1442774,
		1442775,
		1442776,
		1442777,
		1442778,
		1442779,
		1442780,
		1442781,
		1442782,
		1442783,
		1442784,
		1442785,
		1442786,
		1442787,
		1442788,
		1442789,
		1442790,
		1442791,
		1442792,
		1442793,
		1442794,
		1442795,
		1442796,
		1442797,
		1442798,
		1442799,
		1442800,
		1442801,
		1442802,
		1442803,
		1442804,
		1442805,
		1442806,
		1442807,
		1442808,
		1442809,
		1442810,
		1442811,
		1442812,
		1442813,
		1442814,
		1442815,
		1442816,
		1442817,
		1442818,
		1442819,
		1442820,
		1442821,
		1442822,
		1442823,
		1442824,
		1442825,
		1442826,
		1442827,
		1442828,
		1442829,
		1442830,
		1442831,
		1442832,
		1442833,
		1442834,
		1442835,
		1442836,
		1442837,
		1442838,
		1442839,
		1442840,
		1442841,
		1442842,
		1442843,
		1442844,
		1442845,
		1442846,
		1442847,
		1442848,
		1442849,
		1442850,
		1442851,
		1442852,
		1442853,
		1442854,
		1442855,
		1442856,
		1442857,
		1442858,
		1442859,
		1442860,
		1442861,
		1442862,
		1442863,
		1442864,
		1442865,
		1442866,
		1442867,
		1442868,
		1442869,
		1442870,
		1442871,
		1442872,
		1442873,
		1442874,
		1442875,
		1442876,
		1442877,
		1442878,
		1442879,
		1442880,
		1442881,
		1442882,
		1442883,
		1442884,
		1442885,
		1442886,
		1442887,
		1442888,
		1442889,
		1442890,
		1442891,
		1442892,
		1442893,
		1442894,
		1442895,
		1442896,
		1442897,
		1442898,
		1442899}, eth, beaconClient)
	PanicOnError("failed to fetch checkpoint and beacon state", err)
	fmt.Printf("Time taken to fetch checkpoint and beacon state: %v\n", time.Since(startTime))

	startTime = time.Now()
	allValidatorsWithInfoForEigenpod, err := SfFetchMultipleOnchainValidatorInfo(ctx, eth, eigenpodAddress, allValidatorsForEigenpod)
	PanicOnError("failed to fetch validator info", err)
	fmt.Printf("Time taken to fetch validator info: %v\n", time.Since(startTime))

	startTime = time.Now()
	activeValidators, err := SfSelectActiveValidators(eth, eigenpodAddress, allValidatorsWithInfoForEigenpod)
	PanicOnError("failed to find active validators", err)
	fmt.Printf("Time taken to find active validators: %v\n", time.Since(startTime))

	startTime = time.Now()
	checkpointableValidators, err := SfSelectCheckpointableValidators(eth, eigenpodAddress, allValidatorsWithInfoForEigenpod, checkpointTimestamp)
	PanicOnError("failed to find checkpointable validators", err)
	fmt.Printf("Time taken to find checkpointable validators: %v\n", time.Since(startTime))

	startTime = time.Now()
	sumBeaconBalancesWei := IGweiToWei(sfSumValidatorBeaconBalancesGwei(activeValidators))
	sumRestakedBalancesWei := IGweiToWei(sfSumRestakedBalancesGwei(activeValidators))
	PanicOnError("failed to calculate sum of onchain validator balances", err)
	fmt.Printf("Time taken to calculate sum of onchain validator balances: %v\n", time.Since(startTime))

	startTime = time.Now()
	for _, validator := range allValidatorsWithInfoForEigenpod {
		validators[fmt.Sprintf("%d", validator.Index)] = Validator{
			Index:                               validator.Index,
			Status:                              int(validator.Info.Status),
			Slashed:                             validator.Validator.Validator.Slashed,
			PublicKey:                           validator.Validator.Validator.PublicKey.String(),
			IsAwaitingActivationQueue:           validator.Validator.Validator.ActivationEpoch == FAR_FUTURE_EPOCH,
			IsAwaitingWithdrawalCredentialProof: IsAwaitingWithdrawalCredentialProof(validator.Info, validator.Validator.Validator),
			EffectiveBalance:                    uint64(validator.Validator.Validator.EffectiveBalance),
			CurrentBalance:                      uint64(validator.Validator.Balance),
		}
	}
	fmt.Printf("Time taken to process validators: %v\n", time.Since(startTime))

	eigenpodManagerContractAddress, err := eigenPod.EigenPodManager(nil)
	PanicOnError("failed to get manager address", err)

	eigenPodManager, err := onchain.NewEigenPodManager(eigenpodManagerContractAddress, eth)
	PanicOnError("failed to get manager instance", err)

	eigenPodOwner, err := eigenPod.PodOwner(nil)
	PanicOnError("failed to get eigenpod owner", err)

	proofSubmitter, err := eigenPod.ProofSubmitter(nil)
	PanicOnError("failed to get eigenpod proof submitter", err)

	currentOwnerShares, err := eigenPodManager.PodOwnerShares(nil, eigenPodOwner)
	// currentOwnerShares = big.NewInt(0)
	PanicOnError("failed to load pod owner shares", err)
	currentOwnerSharesETH := IweiToEther(currentOwnerShares)
	currentOwnerSharesWei := currentOwnerShares

	withdrawableRestakedExecutionLayerGwei, err := eigenPod.WithdrawableRestakedExecutionLayerGwei(nil)
	PanicOnError("failed to fetch withdrawableRestakedExecutionLayerGwei", err)

	// Estimate the total shares we'll have if we complete an existing checkpoint
	// (or start a new one and complete that).
	//
	// First, we need the change in the pod's native ETH balance since the last checkpoint:
	var nativeETHDeltaWei *big.Int
	mustForceCheckpoint := false

	if checkpointTimestamp != 0 {
		// Change in the pod's native ETH balance (already calculated for us when the checkpoint was started)
		fmt.Printf("pod had a checkpoint\n")
		nativeETHDeltaWei = IGweiToWei(new(big.Int).SetUint64(checkpoint.PodBalanceGwei))

		// Remove already-computed delta from an in-progress checkpoint
		sumRestakedBalancesWei = new(big.Int).Sub(
			sumRestakedBalancesWei,
			IGweiToWei(checkpoint.BalanceDeltasGwei),
		)

		activeCheckpoint = &Checkpoint{
			ProofsRemaining: checkpoint.ProofsRemaining.Uint64(),
			StartedAt:       checkpointTimestamp,
		}
	} else {
		fmt.Printf("pod did not have a checkpoint\n")
		latestPodBalanceWei, err := eth.BalanceAt(ctx, gethCommon.HexToAddress(eigenpodAddress), nil)
		PanicOnError("failed to fetch pod balance", err)

		// We don't have a checkpoint currently, so we need to calculate what
		// checkpoint.PodBalanceGwei would be if we started one now:
		nativeETHDeltaWei = new(big.Int).Sub(
			latestPodBalanceWei,
			IGweiToWei(new(big.Int).SetUint64(withdrawableRestakedExecutionLayerGwei)),
		)

		// Determine whether the checkpoint needs to be started with `--force`
		if nativeETHDeltaWei.Sign() == 0 {
			mustForceCheckpoint = true
		}
	}

	// Next, we need the change in the pod's beacon chain balances since the last
	// checkpoint:
	//
	// beaconETHDeltaWei = sumBeaconBalancesWei - sumRestakedBalancesWei
	beaconETHDeltaWei := new(big.Int).Sub(
		sumBeaconBalancesWei,
		sumRestakedBalancesWei,
	)

	// Sum of these two deltas represents the change in shares after this checkpoint
	totalShareDeltaWei := new(big.Int).Add(
		nativeETHDeltaWei,
		beaconETHDeltaWei,
	)

	// Calculate new total shares by applying delta to current shares
	pendingSharesWei := new(big.Int).Add(
		currentOwnerSharesWei,
		totalShareDeltaWei,
	)

	pendingEth := GweiToEther(WeiToGwei(pendingSharesWei))

	return EigenpodStatus{
		Validators:                     validators,
		ActiveCheckpoint:               activeCheckpoint,
		CurrentTotalSharesETH:          currentOwnerSharesETH,
		TotalSharesAfterCheckpointGwei: WeiToGwei(pendingSharesWei),
		TotalSharesAfterCheckpointETH:  pendingEth,
		NumberValidatorsToCheckpoint:   len(checkpointableValidators),
		PodOwner:                       eigenPodOwner,
		ProofSubmitter:                 proofSubmitter,
		MustForceCheckpoint:            mustForceCheckpoint,
	}
}
