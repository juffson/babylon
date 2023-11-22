package e2e

import (
	"math"
	"math/rand"
	"time"

	"github.com/babylonchain/babylon/btcstaking"
	"github.com/babylonchain/babylon/crypto/eots"
	"github.com/babylonchain/babylon/test/e2e/configurer"
	"github.com/babylonchain/babylon/test/e2e/initialization"
	"github.com/babylonchain/babylon/test/e2e/util"
	"github.com/babylonchain/babylon/testutil/datagen"
	bbn "github.com/babylonchain/babylon/types"
	btcctypes "github.com/babylonchain/babylon/x/btccheckpoint/types"
	bstypes "github.com/babylonchain/babylon/x/btcstaking/types"
	ftypes "github.com/babylonchain/babylon/x/finality/types"
	itypes "github.com/babylonchain/babylon/x/incentive/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/suite"
)

var (
	r   = rand.New(rand.NewSource(time.Now().Unix()))
	net = &chaincfg.SimNetParams
	// BTC validator
	valBTCSK, _, _ = datagen.GenRandomBTCKeyPair(r)
	btcVal         *bstypes.BTCValidator
	// BTC delegation
	delBTCSK, delBTCPK, _ = datagen.GenRandomBTCKeyPair(r)
	// covenant
	covenantSK, _ = btcec.PrivKeyFromBytes(
		[]byte{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1},
	)

	stakingValue = int64(2 * 10e8)

	changeAddress, _ = datagen.GenRandomBTCAddress(r, net)
)

type BTCStakingTestSuite struct {
	suite.Suite

	configurer configurer.Configurer
}

func (s *BTCStakingTestSuite) SetupSuite() {
	s.T().Log("setting up e2e integration test suite...")
	var err error

	// The e2e test flow is as follows:
	//
	// 1. Configure 1 chain with some validator nodes
	// 2. Execute various e2e tests
	s.configurer, err = configurer.NewBTCStakingConfigurer(s.T(), true)
	s.NoError(err)
	err = s.configurer.ConfigureChains()
	s.NoError(err)
	err = s.configurer.RunSetup()
	s.NoError(err)
}

func (s *BTCStakingTestSuite) TearDownSuite() {
	err := s.configurer.ClearResources()
	s.Require().NoError(err)
}

// TestCreateBTCValidatorAndDelegation is an end-to-end test for
// user story 1: user creates BTC validator and BTC delegation
func (s *BTCStakingTestSuite) Test1CreateBTCValidatorAndDelegation() {
	chainA := s.configurer.GetChainConfig(0)
	chainA.WaitUntilHeight(1)
	nonValidatorNode, err := chainA.GetNodeAtIndex(2)
	s.NoError(err)

	/*
		create a random BTC validator on Babylon
	*/
	// NOTE: we use the node's secret key as Babylon secret key for the BTC validator
	btcVal, err = datagen.GenRandomBTCValidatorWithBTCBabylonSKs(r, valBTCSK, nonValidatorNode.SecretKey)
	s.NoError(err)
	nonValidatorNode.CreateBTCValidator(btcVal.BabylonPk, btcVal.BtcPk, btcVal.Pop, btcVal.Description.Moniker, btcVal.Description.Identity, btcVal.Description.Website, btcVal.Description.SecurityContact, btcVal.Description.Details, btcVal.Commission)

	// wait for a block so that above txs take effect
	nonValidatorNode.WaitForNextBlock()

	// query the existence of BTC validator and assert equivalence
	actualBtcVals := nonValidatorNode.QueryBTCValidators()
	s.Len(actualBtcVals, 1)
	s.Equal(util.Cdc.MustMarshal(btcVal), util.Cdc.MustMarshal(actualBtcVals[0]))

	/*
		create a random BTC delegation under this BTC validator
	*/
	// BTC staking params, BTC delegation key pairs and PoP
	params := nonValidatorNode.QueryBTCStakingParams()
	// get covenant BTC PKs
	covenantBTCPKs := []*btcec.PublicKey{}
	for _, covenantPK := range params.CovenantPks {
		covenantBTCPKs = append(covenantBTCPKs, covenantPK.MustToBTCPK())
	}
	// NOTE: we use the node's secret key as Babylon secret key for the BTC delegation
	delBabylonSK := nonValidatorNode.SecretKey
	pop, err := bstypes.NewPoP(delBabylonSK, delBTCSK)
	s.NoError(err)
	// generate staking tx and slashing tx
	stakingTimeBlocks := uint16(math.MaxUint16)
	testStakingInfo := datagen.GenBTCStakingSlashingTx(
		r,
		s.T(),
		net,
		delBTCSK,
		[]*btcec.PublicKey{btcVal.BtcPk.MustToBTCPK()},
		covenantBTCPKs,
		1,
		stakingTimeBlocks,
		stakingValue,
		params.SlashingAddress, changeAddress.String(),
		params.SlashingRate,
	)

	stakingMsgTx := testStakingInfo.StakingTx
	stakingTxHash := stakingMsgTx.TxHash().String()
	stakingSlashingPathInfo, err := testStakingInfo.StakingInfo.SlashingPathSpendInfo()
	s.NoError(err)

	// generate proper delegator sig
	delegatorSig, err := testStakingInfo.SlashingTx.Sign(
		stakingMsgTx,
		0,
		stakingSlashingPathInfo.RevealedLeaf.Script,
		delBTCSK,
		net,
	)
	s.NoError(err)

	// submit staking tx to Bitcoin and get inclusion proof
	currentBtcTip, err := nonValidatorNode.QueryTip()
	s.NoError(err)
	blockWithStakingTx := datagen.CreateBlockWithTransaction(r, currentBtcTip.Header.ToBlockHeader(), stakingMsgTx)
	nonValidatorNode.InsertHeader(&blockWithStakingTx.HeaderBytes)
	// make block k-deep
	for i := 0; i < initialization.BabylonBtcConfirmationPeriod; i++ {
		nonValidatorNode.InsertNewEmptyBtcHeader(r)
	}
	stakingTxInfo := btcctypes.NewTransactionInfoFromSpvProof(blockWithStakingTx.SpvProof)

	// submit the message for creating BTC delegation
	nonValidatorNode.CreateBTCDelegation(
		delBabylonSK.PubKey().(*secp256k1.PubKey),
		bbn.NewBIP340PubKeyFromBTCPK(delBTCPK),
		pop,
		stakingTxInfo,
		btcVal.BtcPk,
		stakingTimeBlocks,
		btcutil.Amount(stakingValue),
		testStakingInfo.SlashingTx,
		delegatorSig,
	)

	// wait for a block so that above txs take effect
	nonValidatorNode.WaitForNextBlock()
	nonValidatorNode.WaitForNextBlock()

	pendingDelSet := nonValidatorNode.QueryBTCValidatorDelegations(btcVal.BtcPk.MarshalHex())
	s.Len(pendingDelSet, 1)
	pendingDels := pendingDelSet[0]
	s.Len(pendingDels.Dels, 1)
	s.Equal(delBTCPK.SerializeCompressed()[1:], pendingDels.Dels[0].BtcPk.MustToBTCPK().SerializeCompressed()[1:])
	s.Nil(pendingDels.Dels[0].CovenantSig)

	// check delegation
	delegation := nonValidatorNode.QueryBtcDelegation(stakingTxHash)
	s.NotNil(delegation)
}

// Test2SubmitCovenantSignature is an end-to-end test for user
// story 2: covenant approves the BTC delegation
func (s *BTCStakingTestSuite) Test2SubmitCovenantSignature() {
	chainA := s.configurer.GetChainConfig(0)
	chainA.WaitUntilHeight(1)
	nonValidatorNode, err := chainA.GetNodeAtIndex(2)
	s.NoError(err)

	// get last BTC delegation
	pendingDelsSet := nonValidatorNode.QueryBTCValidatorDelegations(btcVal.BtcPk.MarshalHex())
	s.Len(pendingDelsSet, 1)
	pendingDels := pendingDelsSet[0]
	s.Len(pendingDels.Dels, 1)
	pendingDel := pendingDels.Dels[0]
	s.Nil(pendingDel.CovenantSig)

	slashingTx := pendingDel.SlashingTx
	stakingTx := pendingDel.StakingTx
	stakingMsgTx, err := bstypes.ParseBtcTx(stakingTx)
	s.NoError(err)
	stakingTxHash := stakingMsgTx.TxHash().String()

	params := nonValidatorNode.QueryBTCStakingParams()
	covenantBTCPKs := []*btcec.PublicKey{}
	for _, covenantPK := range params.CovenantPks {
		covenantBTCPKs = append(covenantBTCPKs, covenantPK.MustToBTCPK())
	}

	validatorBTCPKs := []*btcec.PublicKey{}
	for _, valPK := range pendingDel.ValBtcPkList {
		validatorBTCPKs = append(validatorBTCPKs, valPK.MustToBTCPK())
	}

	stakingInfo, err := btcstaking.BuildStakingInfo(
		pendingDel.BtcPk.MustToBTCPK(),
		validatorBTCPKs,
		covenantBTCPKs,
		params.CovenantQuorum,
		pendingDel.GetStakingTime(),
		btcutil.Amount(pendingDel.TotalSat),
		net,
	)
	s.NoError(err)

	stakingSlashingPathInfo, err := stakingInfo.SlashingPathSpendInfo()
	s.NoError(err)

	/*
		generate and insert new covenant signature, in order to activate the BTC delegation
	*/
	covenantSig, err := slashingTx.Sign(
		stakingMsgTx,
		pendingDel.StakingOutputIdx,
		stakingSlashingPathInfo.RevealedLeaf.Script,
		covenantSK,
		net,
	)
	s.NoError(err)
	nonValidatorNode.AddCovenantSig(btcVal.BtcPk, bbn.NewBIP340PubKeyFromBTCPK(delBTCPK), stakingTxHash, covenantSig)

	// wait for a block so that above txs take effect
	nonValidatorNode.WaitForNextBlock()
	nonValidatorNode.WaitForNextBlock()

	// ensure the BTC delegation has covenant sig now
	activeDelsSet := nonValidatorNode.QueryBTCValidatorDelegations(btcVal.BtcPk.MarshalHex())
	s.Len(activeDelsSet, 1)
	activeDels := activeDelsSet[0]
	s.Len(activeDels.Dels, 1)
	activeDel := activeDels.Dels[0]
	s.NotNil(activeDel.CovenantSig)

	// wait for a block so that above txs take effect and the voting power table
	// is updated in the next block's BeginBlock
	nonValidatorNode.WaitForNextBlock()

	// ensure BTC staking is activated
	activatedHeight := nonValidatorNode.QueryActivatedHeight()
	s.Positive(activatedHeight)
	// ensure BTC validator has voting power at activated height
	currentBtcTip, err := nonValidatorNode.QueryTip()
	s.NoError(err)
	activeBTCVals := nonValidatorNode.QueryActiveBTCValidatorsAtHeight(activatedHeight)
	s.Len(activeBTCVals, 1)
	s.Equal(activeBTCVals[0].VotingPower, activeDels.VotingPower(currentBtcTip.Height, initialization.BabylonBtcFinalizationPeriod))
	s.Equal(activeBTCVals[0].VotingPower, activeDel.VotingPower(currentBtcTip.Height, initialization.BabylonBtcFinalizationPeriod))
}

// Test2CommitPublicRandomnessAndSubmitFinalitySignature is an end-to-end
// test for user story 3: BTC validator commits public randomness and submits
// finality signature, such that blocks can be finalised.
func (s *BTCStakingTestSuite) Test3CommitPublicRandomnessAndSubmitFinalitySignature() {
	chainA := s.configurer.GetChainConfig(0)
	chainA.WaitUntilHeight(1)
	nonValidatorNode, err := chainA.GetNodeAtIndex(2)
	s.NoError(err)

	// get activated height
	activatedHeight := nonValidatorNode.QueryActivatedHeight()
	s.Positive(activatedHeight)

	/*
		commit a number of public randomness since activatedHeight
	*/
	// commit public randomness list
	srList, msgCommitPubRandList, err := datagen.GenRandomMsgCommitPubRandList(r, valBTCSK, activatedHeight, 100)
	s.NoError(err)
	nonValidatorNode.CommitPubRandList(
		msgCommitPubRandList.ValBtcPk,
		msgCommitPubRandList.StartHeight,
		msgCommitPubRandList.PubRandList,
		msgCommitPubRandList.Sig,
	)

	// ensure public randomness list is eventually committed
	nonValidatorNode.WaitForNextBlock()
	var pubRandMap map[uint64]*bbn.SchnorrPubRand
	s.Eventually(func() bool {
		pubRandMap = nonValidatorNode.QueryListPublicRandomness(btcVal.BtcPk)
		return len(pubRandMap) > 0
	}, time.Minute, time.Second*5)
	s.Equal(pubRandMap[activatedHeight].MustMarshal(), msgCommitPubRandList.PubRandList[0].MustMarshal())

	// no reward gauge for BTC validator and delegation yet
	btcValBabylonAddr := sdk.AccAddress(nonValidatorNode.SecretKey.PubKey().Address().Bytes())
	_, err = nonValidatorNode.QueryRewardGauge(btcValBabylonAddr)
	s.Error(err)
	delBabylonAddr := sdk.AccAddress(nonValidatorNode.SecretKey.PubKey().Address().Bytes())
	_, err = nonValidatorNode.QueryRewardGauge(delBabylonAddr)
	s.Error(err)

	/*
		submit finality signature
	*/
	// get block to vote
	blockToVote, err := nonValidatorNode.QueryBlock(int64(activatedHeight))
	s.NoError(err)
	appHash := blockToVote.AppHash

	msgToSign := append(sdk.Uint64ToBigEndian(activatedHeight), appHash...)
	// generate EOTS signature
	sig, err := eots.Sign(valBTCSK, srList[0], msgToSign)
	s.NoError(err)
	eotsSig := bbn.NewSchnorrEOTSSigFromModNScalar(sig)
	// submit finality signature
	nonValidatorNode.AddFinalitySig(btcVal.BtcPk, activatedHeight, appHash, eotsSig)

	// ensure vote is eventually cast
	nonValidatorNode.WaitForNextBlock()
	var votes []bbn.BIP340PubKey
	s.Eventually(func() bool {
		votes = nonValidatorNode.QueryVotesAtHeight(activatedHeight)
		return len(votes) > 0
	}, time.Minute, time.Second*5)
	s.Equal(1, len(votes))
	s.Equal(votes[0].MarshalHex(), btcVal.BtcPk.MarshalHex())
	// once the vote is cast, ensure block is finalised
	finalizedBlock := nonValidatorNode.QueryIndexedBlock(activatedHeight)
	s.NotEmpty(finalizedBlock)
	s.Equal(appHash.Bytes(), finalizedBlock.AppHash)
	finalizedBlocks := nonValidatorNode.QueryListBlocks(ftypes.QueriedBlockStatus_FINALIZED)
	s.NotEmpty(finalizedBlocks)
	s.Equal(appHash.Bytes(), finalizedBlocks[0].AppHash)

	// ensure BTC validator has received rewards after the block is finalised
	btcValRewardGauges, err := nonValidatorNode.QueryRewardGauge(btcValBabylonAddr)
	s.NoError(err)
	btcValRewardGauge, ok := btcValRewardGauges[itypes.BTCValidatorType.String()]
	s.True(ok)
	s.True(btcValRewardGauge.Coins.IsAllPositive())
	// ensure BTC delegation has received rewards after the block is finalised
	btcDelRewardGauges, err := nonValidatorNode.QueryRewardGauge(delBabylonAddr)
	s.NoError(err)
	btcDelRewardGauge, ok := btcDelRewardGauges[itypes.BTCDelegationType.String()]
	s.True(ok)
	s.True(btcDelRewardGauge.Coins.IsAllPositive())
}

func (s *BTCStakingTestSuite) Test4WithdrawReward() {
	chainA := s.configurer.GetChainConfig(0)
	nonValidatorNode, err := chainA.GetNodeAtIndex(2)
	s.NoError(err)

	// BTC validator balance before withdraw
	btcValBabylonAddr := sdk.AccAddress(nonValidatorNode.SecretKey.PubKey().Address().Bytes())
	delBabylonAddr := sdk.AccAddress(nonValidatorNode.SecretKey.PubKey().Address().Bytes())
	btcValBalance, err := nonValidatorNode.QueryBalances(btcValBabylonAddr.String())
	s.NoError(err)
	// BTC validator reward gauge should not be fully withdrawn
	btcValRgs, err := nonValidatorNode.QueryRewardGauge(btcValBabylonAddr)
	s.NoError(err)
	btcValRg := btcValRgs[itypes.BTCValidatorType.String()]
	s.T().Logf("BTC validator's withdrawable reward before withdrawing: %s", btcValRg.GetWithdrawableCoins().String())
	s.False(btcValRg.IsFullyWithdrawn())

	// withdraw BTC validator reward
	nonValidatorNode.WithdrawReward(itypes.BTCValidatorType.String(), initialization.ValidatorWalletName)
	nonValidatorNode.WaitForNextBlock()

	// balance after withdrawing BTC validator reward
	btcValBalance2, err := nonValidatorNode.QueryBalances(btcValBabylonAddr.String())
	s.NoError(err)
	s.T().Logf("btcValBalance2: %s; btcValBalance: %s", btcValBalance2.String(), btcValBalance.String())
	s.True(btcValBalance2.IsAllGT(btcValBalance))
	// BTC validator reward gauge should be fully withdrawn now
	btcValRgs2, err := nonValidatorNode.QueryRewardGauge(btcValBabylonAddr)
	s.NoError(err)
	btcValRg2 := btcValRgs2[itypes.BTCValidatorType.String()]
	s.T().Logf("BTC validator's withdrawable reward after withdrawing: %s", btcValRg2.GetWithdrawableCoins().String())
	s.True(btcValRg2.IsFullyWithdrawn())

	// BTC delegation balance before withdraw
	btcDelBalance, err := nonValidatorNode.QueryBalances(delBabylonAddr.String())
	s.NoError(err)
	// BTC delegation reward gauge should not be fully withdrawn
	btcDelRgs, err := nonValidatorNode.QueryRewardGauge(delBabylonAddr)
	s.NoError(err)
	btcDelRg := btcDelRgs[itypes.BTCDelegationType.String()]
	s.T().Logf("BTC delegation's withdrawable reward before withdrawing: %s", btcDelRg.GetWithdrawableCoins().String())
	s.False(btcDelRg.IsFullyWithdrawn())

	// withdraw BTC delegation reward
	nonValidatorNode.WithdrawReward(itypes.BTCDelegationType.String(), initialization.ValidatorWalletName)
	nonValidatorNode.WaitForNextBlock()

	// balance after withdrawing BTC delegation reward
	btcDelBalance2, err := nonValidatorNode.QueryBalances(delBabylonAddr.String())
	s.NoError(err)
	s.T().Logf("btcDelBalance2: %s; btcDelBalance: %s", btcDelBalance2.String(), btcDelBalance.String())
	s.True(btcDelBalance2.IsAllGT(btcDelBalance))
	// BTC delegation reward gauge should be fully withdrawn now
	btcDelRgs2, err := nonValidatorNode.QueryRewardGauge(delBabylonAddr)
	s.NoError(err)
	btcDelRg2 := btcDelRgs2[itypes.BTCDelegationType.String()]
	s.T().Logf("BTC delegation's withdrawable reward after withdrawing: %s", btcDelRg2.GetWithdrawableCoins().String())
	s.True(btcDelRg2.IsFullyWithdrawn())
}

// Test5SubmitStakerUnbonding is an end-to-end test for user unbonding
func (s *BTCStakingTestSuite) Test5SubmitStakerUnbonding() {
	chainA := s.configurer.GetChainConfig(0)
	chainA.WaitUntilHeight(1)
	nonValidatorNode, err := chainA.GetNodeAtIndex(2)
	s.NoError(err)
	// wait for a block so that above txs take effect
	nonValidatorNode.WaitForNextBlock()

	activeDelsSet := nonValidatorNode.QueryBTCValidatorDelegations(btcVal.BtcPk.MarshalHex())
	s.Len(activeDelsSet, 1)
	activeDels := activeDelsSet[0]
	s.Len(activeDels.Dels, 1)
	activeDel := activeDels.Dels[0]
	s.NotNil(activeDel.CovenantSig)

	// params for covenantPk and slashing address
	params := nonValidatorNode.QueryBTCStakingParams()
	// get covenant BTC PKs
	covenantBTCPKs := []*btcec.PublicKey{}
	for _, covenantPK := range params.CovenantPks {
		covenantBTCPKs = append(covenantBTCPKs, covenantPK.MustToBTCPK())
	}

	validatorBTCPKs := []*btcec.PublicKey{}
	for _, valPK := range activeDel.ValBtcPkList {
		validatorBTCPKs = append(validatorBTCPKs, valPK.MustToBTCPK())
	}

	stakingMsgTx, err := bstypes.ParseBtcTx(activeDel.StakingTx)
	s.NoError(err)
	stakingTxHash := stakingMsgTx.TxHash().String()
	stakingTxChainHash, err := chainhash.NewHashFromStr(stakingTxHash)
	s.NoError(err)

	fee := int64(1000)
	testUnbondingInfo := datagen.GenBTCUnbondingSlashingTx(
		r,
		s.T(),
		net,
		delBTCSK,
		validatorBTCPKs,
		covenantBTCPKs,
		params.CovenantQuorum,
		wire.NewOutPoint(stakingTxChainHash, uint32(activeDel.StakingOutputIdx)),
		initialization.BabylonBtcFinalizationPeriod+1,
		stakingValue-fee,
		params.SlashingAddress, changeAddress.String(),
		params.SlashingRate,
	)
	s.NoError(err)

	unbondingTxMsg := testUnbondingInfo.UnbondingTx

	unbondingTxSlashingPathInfo, err := testUnbondingInfo.UnbondingInfo.SlashingPathSpendInfo()
	s.NoError(err)

	slashingTxSig, err := testUnbondingInfo.SlashingTx.Sign(
		unbondingTxMsg,
		0,
		unbondingTxSlashingPathInfo.RevealedLeaf.Script,
		delBTCSK,
		net,
	)
	s.NoError(err)

	// submit the message for creating BTC undelegation
	nonValidatorNode.CreateBTCUndelegation(
		unbondingTxMsg,
		testUnbondingInfo.SlashingTx,
		initialization.BabylonBtcFinalizationPeriod+1,
		btcutil.Amount(stakingValue-fee),
		slashingTxSig,
	)
	// wait for a block so that above txs take effect
	nonValidatorNode.WaitForNextBlock()

	valDelegations := nonValidatorNode.QueryBTCValidatorDelegations(btcVal.BtcPk.MarshalHex())
	s.Len(valDelegations, 1)
	s.Len(valDelegations[0].Dels, 1)
	delegation := valDelegations[0].Dels[0]
	s.NotNil(delegation.BtcUndelegation)
}

// Test6SubmitStakerUnbonding is an end-to-end test for covenant and validator submitting signatures
// for unbonding transaction
func (s *BTCStakingTestSuite) Test6SubmitUnbondingSignatures() {
	chainA := s.configurer.GetChainConfig(0)
	chainA.WaitUntilHeight(1)
	nonValidatorNode, err := chainA.GetNodeAtIndex(2)
	s.NoError(err)
	// wait for a block so that above txs take effect
	nonValidatorNode.WaitForNextBlock()

	allDelegations := nonValidatorNode.QueryBTCValidatorDelegations(btcVal.BtcPk.MarshalHex())
	s.Len(allDelegations, 1)
	delegatorDelegations := allDelegations[0]
	s.Len(delegatorDelegations.Dels, 1)
	delegation := delegatorDelegations.Dels[0]

	s.NotNil(delegation.BtcUndelegation)
	s.Nil(delegation.BtcUndelegation.CovenantUnbondingSig)
	s.Nil(delegation.BtcUndelegation.CovenantSlashingSig)

	// params for covenantPk and slashing address
	params := nonValidatorNode.QueryBTCStakingParams()
	// get covenant BTC PKs
	covenantBTCPKs := []*btcec.PublicKey{}
	for _, covenantPK := range params.CovenantPks {
		covenantBTCPKs = append(covenantBTCPKs, covenantPK.MustToBTCPK())
	}

	validatorBTCPKs := []*btcec.PublicKey{}
	for _, valPK := range delegation.ValBtcPkList {
		validatorBTCPKs = append(validatorBTCPKs, valPK.MustToBTCPK())
	}

	stakingInfo, err := btcstaking.BuildStakingInfo(
		delegation.BtcPk.MustToBTCPK(),
		validatorBTCPKs,
		covenantBTCPKs,
		params.CovenantQuorum,
		delegation.GetStakingTime(),
		btcutil.Amount(delegation.TotalSat),
		net,
	)
	s.NoError(err)

	unbondingTx, err := bstypes.ParseBtcTx(delegation.BtcUndelegation.UnbondingTx)
	s.NoError(err)
	stakingTx, err := bstypes.ParseBtcTx(delegation.StakingTx)
	s.NoError(err)
	stakingTxHash := stakingTx.TxHash().String()

	unbondingInfo, err := btcstaking.BuildUnbondingInfo(
		delegation.BtcPk.MustToBTCPK(),
		validatorBTCPKs,
		covenantBTCPKs,
		params.CovenantQuorum,
		uint16(delegation.BtcUndelegation.UnbondingTime),
		btcutil.Amount(unbondingTx.TxOut[0].Value),
		net,
	)
	s.NoError(err)

	// Next send covenant signatures.
	// First covenant signature on unbonding tx
	stakingTxUnbondigPathInfo, err := stakingInfo.UnbondingPathSpendInfo()
	s.NoError(err)

	covenantUnbondingSignature, err := btcstaking.SignTxWithOneScriptSpendInputStrict(
		unbondingTx,
		stakingTx,
		delegation.StakingOutputIdx,
		stakingTxUnbondigPathInfo.RevealedLeaf.Script,
		covenantSK,
		net,
	)
	s.NoError(err)

	covenantUnbondingSig := bbn.NewBIP340SignatureFromBTCSig(covenantUnbondingSignature)

	// Next covenant signature on unbonding slashing tx
	unbondingTxSlashingPath, err := unbondingInfo.SlashingPathSpendInfo()
	s.NoError(err)

	covenantSlashingSig, err := delegation.BtcUndelegation.SlashingTx.Sign(
		unbondingTx,
		0,
		unbondingTxSlashingPath.RevealedLeaf.Script,
		covenantSK,
		net,
	)
	s.NoError(err)
	nonValidatorNode.AddCovenantUnbondingSigs(
		btcVal.BtcPk,
		bbn.NewBIP340PubKeyFromBTCPK(delBTCPK), stakingTxHash, &covenantUnbondingSig, covenantSlashingSig)
	nonValidatorNode.WaitForNextBlock()
	nonValidatorNode.WaitForNextBlock()

	// Check all signatures are properly registered
	allDelegationsWithSigs := nonValidatorNode.QueryBTCValidatorDelegations(btcVal.BtcPk.MarshalHex())
	s.Len(allDelegationsWithSigs, 1)
	delegationWithSigs := allDelegationsWithSigs[0].Dels[0]
	s.NotNil(delegationWithSigs.BtcUndelegation)
	s.NotNil(delegationWithSigs.BtcUndelegation.CovenantUnbondingSig)
	s.NotNil(delegationWithSigs.BtcUndelegation.CovenantSlashingSig)
	btcTip, err := nonValidatorNode.QueryTip()
	s.NoError(err)
	s.Equal(
		bstypes.BTCDelegationStatus_UNBONDED,
		delegationWithSigs.GetStatus(btcTip.Height, initialization.BabylonBtcFinalizationPeriod),
	)
}
