package integration

import (
	"fmt"

	clienttypes "github.com/cosmos/ibc-go/v10/modules/core/02-client/types"
	channeltypes "github.com/cosmos/ibc-go/v10/modules/core/04-channel/types"

	"cosmossdk.io/core/comet"
	"cosmossdk.io/math"

	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkaddress "github.com/cosmos/cosmos-sdk/types/address"
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/crypto/ed25519"
	tmtypes "github.com/cometbft/cometbft/types"

	keepertestutil "github.com/cosmos/interchain-security/v7/testutil/keeper"
	providertypes "github.com/cosmos/interchain-security/v7/x/ccv/provider/types"
	ccv "github.com/cosmos/interchain-security/v7/x/ccv/types"
)

// TestRelayAndApplyDowntimePacket tests that downtime slash packets can be properly relayed from consumer to provider,
// handled by provider, with a VSC and jailing eventually effective on consumer and provider.
// @Long Description@
// * Set up CCV channels and retrieve consumer validators.
// * Select a validator and create its consensus address.
// * Retrieve the provider consensus address that corresponds to the consumer consensus address of the validator.
// * The validator's current state is also retrieved, including its token balance,
// * Set validator's signing information is to ensure it will be jailed for downtime.
// * Create the slashing packet and send it from the consumer chain to the provider chain with a specified timeout.
// * Receive the packet and verify that the validator was removed from the provider validator set.
// * Relay VSC packets from the provider chain to each consumer chain and verify that the consumer chains correctly process these packets.
// * Check the validator's balance and status on the provider chain to ensure it was jailed correctly but not slashed,
// and its unjailing time is updated.
// * Reset the outstanding downtime flag on the consumer chain, and ensure that the consumer
// chain acknowledges receipt of the packet from the provider chain.
//
// Note: This method does not test the actual slash packet sending logic for downtime
// and double-signing, see TestValidatorDowntime and TestValidatorDoubleSigning for
// those types of tests.
func (s *CCVTestSuite) TestRelayAndApplyDowntimePacket() {
	// Setup CCV channel for all instantiated consumers
	s.SetupAllCCVChannels()

	validatorsPerChain := len(s.consumerChain.Vals.Validators)

	providerStakingKeeper := s.providerApp.GetTestStakingKeeper()
	providerSlashingKeeper := s.providerApp.GetTestSlashingKeeper()
	providerKeeper := s.providerApp.GetProviderKeeper()
	firstConsumerKeeper := s.getFirstBundle().GetKeeper()

	// pick first consumer validator
	tmVal := s.consumerChain.Vals.Validators[0]
	val, err := tmVal.ToProto()
	s.Require().NoError(err)
	pubkey, err := cryptocodec.FromCmtProtoPublicKey(val.GetPubKey())
	s.Require().Nil(err)
	consumerConsAddr := providertypes.NewConsumerConsAddress(sdk.GetConsAddress(pubkey))
	// map consumer consensus address to provider consensus address
	providerConsAddr, found := providerKeeper.GetValidatorByConsumerAddr(
		s.providerCtx(),
		s.getFirstBundle().ConsumerId,
		consumerConsAddr,
	)
	s.Require().True(found)

	stakingVal, err := providerStakingKeeper.GetValidatorByConsAddr(s.providerCtx(), providerConsAddr.ToSdkConsAddr())
	s.Require().NoError(err)
	valOldBalance := stakingVal.Tokens

	// Setup first val with mapped consensus address to be jailed on provider by setting signing info
	// convert validator to TM type
	pk, err := stakingVal.ConsPubKey()
	s.Require().NoError(err)
	tmPk, err := cryptocodec.ToCmtPubKeyInterface(pk)
	s.Require().NoError(err)
	s.setDefaultValSigningInfo(*tmtypes.NewValidator(tmPk, stakingVal.ConsensusPower(sdk.DefaultPowerReduction)))

	// Send slash packet from the first consumer chain
	var (
		timeoutHeight    = clienttypes.Height{}
		timeoutTimestamp = uint64(s.getFirstBundle().GetCtx().BlockTime().Add(ccv.DefaultCCVTimeoutPeriod).UnixNano())
	)
	slashPacket := s.constructSlashPacketFromConsumer(s.getFirstBundle(), *tmVal, stakingtypes.Infraction_INFRACTION_DOWNTIME, 1)
	sequence, err := s.getFirstBundle().Path.EndpointA.SendPacket(timeoutHeight, timeoutTimestamp, slashPacket.GetData())
	s.Require().NoError(err)

	// Set outstanding slashing flag for first consumer, it's important to use the consumer's cons addr here
	firstConsumerKeeper.SetOutstandingDowntime(s.consumerCtx(), consumerConsAddr.ToSdkConsAddr())

	// Note: RecvPacket advances two blocks. Let's say the provider is currently at height N.
	// The received slash packet will be handled during N. The staking module will then register
	// a validator update from that packet during the endblocker of N. Then the ccv module sends
	// VSC packets to each consumer during the endblocker of N (note ccv endblocker runs after staking).
	// The new validator set will be committed to in block N+1, and will be in effect
	// for the provider during block N+2.

	valsetUpdateIdN := providerKeeper.GetValidatorSetUpdateId(s.providerCtx())

	// receive the slash packet on the provider chain. RecvPacket() calls the provider endblocker twice
	packet := s.newPacketFromConsumer(slashPacket.GetData(), sequence, s.getFirstBundle().Path, timeoutHeight, timeoutTimestamp)
	heightBefore := s.providerCtx().BlockHeight()
	err = s.path.EndpointB.RecvPacket(packet)
	heightAfter := s.providerCtx().BlockHeight()
	s.Require().NoError(err)
	s.Require().Equal(heightBefore+2, heightAfter)

	// this call was added starging cosmos-sdk v0.50.x
	s.nextEpoch()

	s.nextEpoch()

	// Confirm the valset update Id was incremented twice on provider,
	// since an epoch has passed.
	s.Require().Equal(valsetUpdateIdN+2,
		providerKeeper.GetValidatorSetUpdateId(s.providerCtx()))

	// check that the validator was removed from the provider validator set by N + 2
	s.Require().Len(s.providerChain.Vals.Validators, validatorsPerChain-1)

	// VSC packets should have been sent from provider during block N to each consumer
	expectedSentValsetUpdateId := valsetUpdateIdN
	for _, bundle := range s.consumerBundles {
		// Relay VSC packets from provider to each consumer
		relayAllCommittedPackets(s, s.providerChain, bundle.Path,
			ccv.ProviderPortID, bundle.Path.EndpointB.ChannelID, 1)

		// check that each consumer updated its VSC ID for the subsequent not committed block ctx.BlockHeight()
		consumerKeeper := bundle.GetKeeper()
		ctx := bundle.GetCtx()
		actualValsetUpdateID := consumerKeeper.GetHeightValsetUpdateID(
			ctx, uint64(ctx.BlockHeight()))
		s.Require().Equal(expectedSentValsetUpdateId, actualValsetUpdateID)

		// check that jailed validator was removed from each consumer validator set
		s.Require().Len(bundle.Chain.Vals.Validators, validatorsPerChain-1)
	}

	// Get staking keeper's validator obj after the relayed slash packet
	stakingValAfter, err := providerStakingKeeper.GetValidatorByConsAddr(s.providerCtx(), providerConsAddr.ToSdkConsAddr())
	s.Require().NoError(err)

	// check that the validator's tokens were NOT slashed on provider
	valNewBalance := stakingValAfter.GetTokens()
	s.Require().Equal(valOldBalance, valNewBalance)

	// Get signing info for the validator
	valSignInfo, err := providerSlashingKeeper.GetValidatorSigningInfo(s.providerCtx(), providerConsAddr.ToSdkConsAddr())
	s.Require().NoError(err)

	// check that the validator is successfully jailed on provider
	s.Require().True(stakingValAfter.Jailed)
	s.Require().Equal(stakingValAfter.Status, stakingtypes.Unbonding)

	// check that the validator's unjailing time is updated on provider
	s.Require().True(valSignInfo.JailedUntil.After(s.providerCtx().BlockHeader().Time))

	// check that the outstanding slashing flag is reset on first consumer,
	// since that consumer originally sent the slash packet.
	// It's important to use the consumer's cons addr here.
	pFlag := firstConsumerKeeper.OutstandingDowntime(s.consumerCtx(), consumerConsAddr.ToSdkConsAddr())
	s.Require().False(pFlag)

	// Check that first consumer can recv ack from provider.
	// Provider has returned SlashPacketHandledResult.
	ack := channeltypes.NewResultAcknowledgement(ccv.SlashPacketHandledResult)
	err = s.getFirstBundle().Path.EndpointA.AcknowledgePacket(packet, ack.Acknowledgement())
	s.Require().NoError(err)
}

// TestSlashPacketAcknowledgement tests the handling of a slash packet acknowledgement.
// @Long Description@
// * Set up a provider and consumer chain, with channel initialization between them performed.
// * Send a slash packet with randomized fields from the consumer to the provider.
// * The provider processes the packet
func (s *CCVTestSuite) TestSlashPacketAcknowledgement() {
	providerKeeper := s.providerApp.GetProviderKeeper()
	consumerKeeper := s.consumerApp.GetConsumerKeeper()

	s.SetupCCVChannel(s.path)
	s.SetupTransferChannel()

	// Mock a proper slash packet from consumer
	spd := keepertestutil.GetNewSlashPacketData()

	// We don't want truly randomized fields, infraction needs to be specified
	if spd.Infraction == stakingtypes.Infraction_INFRACTION_UNSPECIFIED {
		spd.Infraction = stakingtypes.Infraction_INFRACTION_DOUBLE_SIGN
	}
	cpd := ccv.NewConsumerPacketData(ccv.SlashPacket,
		&ccv.ConsumerPacketData_SlashPacketData{
			SlashPacketData: &spd,
		},
	)
	packet := s.newPacketFromConsumer(cpd.GetBytes(), // Consumer always sends v1 packet data
		1, s.path, clienttypes.Height{}, 0)

	// Map infraction height on provider so validation passes and provider returns valid ack result
	providerKeeper.SetValsetUpdateBlockHeight(s.providerCtx(), spd.ValsetUpdateId, 47923)

	ackResult, err := providerKeeper.OnRecvSlashPacket(s.providerCtx(), packet, spd)
	s.Require().NotNil(ackResult)
	s.Require().NoError(err)
	exportedAck := channeltypes.NewResultAcknowledgement(ackResult)

	// Unmarshal ack to struct that's compatible with consumer. IBC does this automatically
	ack := channeltypes.Acknowledgement{}
	err = channeltypes.SubModuleCdc.UnmarshalJSON(exportedAck.Acknowledgement(), &ack)
	s.Require().NoError(err)

	err = consumerKeeper.OnAcknowledgementPacket(s.consumerCtx(), packet, ack)
	s.Require().NoError(err)

	err = consumerKeeper.OnAcknowledgementPacket(s.consumerCtx(), packet, ccv.NewErrorAcknowledgementWithLog(s.consumerCtx(), fmt.Errorf("another error")))
	s.Require().Error(err)
}

// TestHandleSlashPacketDowntime tests the handling of a downtime related slash packet, with integration tests.
// @Long Description@
// * Retrieve a validator from provider chain's validators and checks if it's bonded.
// * Set the signing information for the validator.
// * The provider processes the downtime slashing packet from the consumer.
// * Check that the validator has been jailed as a result of the downtime slashing packet being processed.
// * Verify that the validator’s signing information is updated and that the jailing duration is set correctly.
//
// Note that only downtime slash packets are processed by HandleSlashPacket.
func (suite *CCVTestSuite) TestHandleSlashPacketDowntime() {
	providerKeeper := suite.providerApp.GetProviderKeeper()
	providerSlashingKeeper := suite.providerApp.GetTestSlashingKeeper()
	providerStakingKeeper := suite.providerApp.GetTestStakingKeeper()

	tmVal := suite.providerChain.Vals.Validators[0]
	consAddr := sdk.ConsAddress(tmVal.Address)

	// check that validator bonded status
	validator, err := providerStakingKeeper.GetValidatorByConsAddr(suite.providerCtx(), consAddr)
	suite.Require().NoError(err)
	suite.Require().Equal(stakingtypes.Bonded, validator.GetStatus())

	// set init VSC id for chain0
	providerKeeper.SetInitChainHeight(suite.providerCtx(), suite.getFirstBundle().ConsumerId, uint64(suite.providerCtx().BlockHeight()))

	// set validator signing-info
	providerSlashingKeeper.SetValidatorSigningInfo(
		suite.providerCtx(),
		consAddr,
		slashingtypes.ValidatorSigningInfo{Address: consAddr.String()},
	)

	providerKeeper.HandleSlashPacket(suite.providerCtx(), suite.getFirstBundle().ConsumerId,
		*ccv.NewSlashPacketData(
			abci.Validator{Address: tmVal.Address, Power: 0},
			uint64(0),
			stakingtypes.Infraction_INFRACTION_DOWNTIME,
		),
	)

	// verify that validator is jailed in the staking and slashing modules' states
	suite.Require().True(providerStakingKeeper.IsValidatorJailed(suite.providerCtx(), consAddr))

	signingInfo, _ := providerSlashingKeeper.GetValidatorSigningInfo(suite.providerCtx(), consAddr)
	jailDuration, err := providerSlashingKeeper.DowntimeJailDuration(suite.providerCtx())
	suite.Require().NoError(err)
	suite.Require().Equal(suite.providerCtx().BlockTime().Add(jailDuration), signingInfo.JailedUntil)
}

// TestOnRecvSlashPacketErrors tests errors for the OnRecvSlashPacket method in an integration testing setting.
// @Long Description@
// * Set up all CCV channels and expect panic if the channel is not established via dest channel of packet.
// * After the correct channelID is added to the packet, a panic shouldn't occur anymore.
// * Create an instance of SlashPacketData and then verify correct processing and error handling
// for slashing packets received by the provider chain.
// TODO: Move to unit tests.
func (suite *CCVTestSuite) TestOnRecvSlashPacketErrors() {
	providerKeeper := suite.providerApp.GetProviderKeeper()
	firstBundle := suite.getFirstBundle()

	suite.SetupAllCCVChannels()

	// sync contexts block height
	ctx := suite.providerCtx()

	// Expect panic if ccv channel is not established via dest channel of packet
	suite.Panics(func() {
		_, _ = providerKeeper.OnRecvSlashPacket(ctx, channeltypes.Packet{}, ccv.SlashPacketData{})
	})

	// Add correct channelID to packet. Now we will not panic anymore.
	packet := channeltypes.Packet{DestinationChannel: firstBundle.Path.EndpointB.ChannelID}
	suite.NotPanics(func() {
		_, _ = providerKeeper.OnRecvSlashPacket(ctx, packet, ccv.SlashPacketData{})
	})

	// Check Validate for SlashPacket data
	validAddress := ed25519.GenPrivKey().PubKey().Address()
	slashPacketData := ccv.NewSlashPacketData(
		abci.Validator{
			Address: validAddress,
			Power:   int64(1),
		}, uint64(0), stakingtypes.Infraction_INFRACTION_DOWNTIME,
	)

	// Expect an error if validator address is too long
	slashPacketData.Validator.Address = make([]byte, sdkaddress.MaxAddrLen+1)
	_, err := providerKeeper.OnRecvSlashPacket(ctx, packet, *slashPacketData)
	suite.Require().Error(err, "validating SlashPacket data should fail - invalid validator address")

	// Expect an error if validator power is zero
	slashPacketData.Validator.Address = validAddress
	slashPacketData.Validator.Power = 0
	_, err = providerKeeper.OnRecvSlashPacket(ctx, packet, *slashPacketData)
	suite.Require().Error(err, "validating SlashPacket data should fail - invalid validator power")

	// Expect an error if the infraction type is unspecified
	slashPacketData.Validator.Power = 1
	slashPacketData.Infraction = stakingtypes.Infraction_INFRACTION_UNSPECIFIED
	_, err = providerKeeper.OnRecvSlashPacket(ctx, packet, *slashPacketData)
	suite.Require().Error(err, "validating SlashPacket data should fail - invalid infraction type")

	// Restore slashPacketData to be valid
	slashPacketData.Infraction = stakingtypes.Infraction_INFRACTION_DOWNTIME

	// Check ValidateSlashPacket
	// Expect an error if a mapping of the infraction height cannot be found;
	// just set the vscID of the slash packet to the latest mapped vscID +1
	valsetUpdateBlockHeights := providerKeeper.GetAllValsetUpdateBlockHeights(ctx)
	latestMappedValsetUpdateId := valsetUpdateBlockHeights[len(valsetUpdateBlockHeights)-1].ValsetUpdateId
	slashPacketData.ValsetUpdateId = latestMappedValsetUpdateId + 1
	_, err = providerKeeper.OnRecvSlashPacket(ctx, packet, *slashPacketData)
	suite.Require().Error(err, "ValidateSlashPacket should fail - no infraction height mapping")

	// Restore slashPacketData to be valid
	slashPacketData.ValsetUpdateId = latestMappedValsetUpdateId

	// Expect no error if validator does not exist
	_, err = providerKeeper.OnRecvSlashPacket(ctx, packet, *slashPacketData)
	suite.Require().NoError(err)

	// Check expected behavior for handling SlashPackets for double signing infractions
	slashPacketData.Infraction = stakingtypes.Infraction_INFRACTION_DOUBLE_SIGN
	ackResult, err := providerKeeper.OnRecvSlashPacket(ctx, packet, *slashPacketData)
	suite.Require().NoError(err)
	suite.Require().Equal(ccv.V1Result, ackResult, "expected successful ack")

	// Check expected behavior for handling SlashPackets for downtime infractions
	slashPacketData.Infraction = stakingtypes.Infraction_INFRACTION_DOWNTIME

	// Expect the packet to bounce if the slash meter is negative
	providerKeeper.SetSlashMeter(ctx, math.NewInt(-1))
	// Only reaches the bouncing code if it fails in the check that chain is not launched and in the check that
	// the validator is not a consumer validator.
	providerKeeper.SetConsumerPhase(suite.providerCtx(), firstBundle.ConsumerId, providertypes.CONSUMER_PHASE_LAUNCHED)
	err = providerKeeper.SetConsumerValidator(ctx, firstBundle.ConsumerId, providertypes.ConsensusValidator{
		ProviderConsAddr: validAddress,
	})
	suite.Require().NoError(err)
	ackResult, err = providerKeeper.OnRecvSlashPacket(ctx, packet, *slashPacketData)
	suite.Require().NoError(err)
	suite.Require().Equal(ccv.SlashPacketBouncedResult, ackResult, "expected bounced result")

	// Expect packet not to bounce if the chain is stopped
	providerKeeper.SetSlashMeter(ctx, math.NewInt(-1))
	providerKeeper.SetConsumerPhase(suite.providerCtx(), firstBundle.ConsumerId, providertypes.CONSUMER_PHASE_STOPPED)
	ackResult, err = providerKeeper.OnRecvSlashPacket(ctx, packet, *slashPacketData)
	suite.Require().NoError(err)
	suite.Require().Equal(ccv.SlashPacketHandledResult, ackResult, "expected successful ack")

	// Expect packet not to bounce if the chain is launched but the validator is not a consumer validator
	providerKeeper.SetSlashMeter(ctx, math.NewInt(-1))
	providerKeeper.SetConsumerPhase(suite.providerCtx(), firstBundle.ConsumerId, providertypes.CONSUMER_PHASE_LAUNCHED)
	providerKeeper.DeleteConsumerValidator(ctx, firstBundle.ConsumerId, providertypes.NewProviderConsAddress(sdk.ConsAddress(validAddress)))
	ackResult, err = providerKeeper.OnRecvSlashPacket(ctx, packet, *slashPacketData)
	suite.Require().NoError(err)
	suite.Require().Equal(ccv.SlashPacketHandledResult, ackResult, "expected successful ack")

	// Also test what happens if the chain is launched but we have a consumer validator. In this case the check that the
	// chain is launched and the validator is not a consumer validator fails, and hence the packet bounces due to the
	// negative slash meter.
	err = providerKeeper.SetConsumerValidator(ctx, firstBundle.ConsumerId, providertypes.ConsensusValidator{
		ProviderConsAddr: validAddress,
	})
	suite.Require().NoError(err)
	ackResult, err = providerKeeper.OnRecvSlashPacket(ctx, packet, *slashPacketData)
	suite.Require().NoError(err)
	suite.Require().Equal(ccv.SlashPacketBouncedResult, ackResult, "expected bounced result")

	// Expect the packet to be handled if the slash meter is positive
	providerKeeper.SetSlashMeter(ctx, math.NewInt(0))
	ackResult, err = providerKeeper.OnRecvSlashPacket(ctx, packet, *slashPacketData)
	suite.Require().NoError(err)
	suite.Require().Equal(ccv.SlashPacketHandledResult, ackResult, "expected successful ack")
}

// TestValidatorDowntime tests if a slash packet is sent and if the outstanding slashing flag is switched
// when a validator has downtime on the slashing module.
// @Long Description@
// * Set up all CCV channel and send an empty VSC packet, then retrieve the address of a validator.
// * Validator signs blocks for the duration of the signedBlocksWindow and a slash packet is constructed to be sent and committed.
// * Simulate the validator missing blocks and then verify that the validator is jailed and the jailed time is correctly updated.
// * Ensure that the missed block counters are reset.
// * Check that there is a pending slash packet in the queue, and then send the pending packets.
// * Check if slash record is created and verify that the consumer queue still contains the packet since no
// acknowledgment has been received from the provider.
// * Verify that the slash packet was sent and check that the outstanding slashing flag prevents the jailed validator to keep missing block.
func (suite *CCVTestSuite) TestValidatorDowntime() {
	// initial setup
	suite.SetupCCVChannel(suite.path)
	suite.SendEmptyVSCPacket()

	consumerKeeper := suite.consumerApp.GetConsumerKeeper()
	consumerSlashingKeeper := suite.consumerApp.GetTestSlashingKeeper()
	consumerIBCKeeper := suite.consumerApp.GetIBCKeeper()

	// sync suite context after CCV channel is established
	ctx := suite.consumerCtx()

	channelID := suite.path.EndpointA.ChannelID

	// pick a cross-chain validator
	vals := consumerKeeper.GetAllCCValidator(ctx)
	consAddr := sdk.ConsAddress(vals[0].Address)

	// save next sequence before sending a slash packet
	seq, ok := consumerIBCKeeper.ChannelKeeper.GetNextSequenceSend(
		ctx, ccv.ConsumerPortID, channelID)
	suite.Require().True(ok)

	// Sign 100 blocks (default value for slashing.SignedBlocksWindow param).
	valPower := int64(1)
	height := int64(0)
	signedBlocksWindow, err := consumerSlashingKeeper.SignedBlocksWindow(ctx)
	suite.Require().NoError(err)
	for ; height < signedBlocksWindow; height++ {
		ctx = ctx.WithBlockHeight(height)
		consumerSlashingKeeper.HandleValidatorSignature(ctx, vals[0].Address, valPower, comet.BlockIDFlagCommit)
	}

	minSigned, err := consumerSlashingKeeper.MinSignedPerWindow(ctx)
	suite.Require().NoError(err)
	missedBlockThreshold := (2 * signedBlocksWindow) - minSigned
	ctx = suite.consumerCtx()

	// construct slash packet to be sent and get its commit
	packetData := ccv.NewSlashPacketData(
		abci.Validator{Address: vals[0].Address, Power: valPower},
		// get the VSC ID mapping the infraction height
		consumerKeeper.GetHeightValsetUpdateID(ctx, uint64(missedBlockThreshold-sdk.ValidatorUpdateDelay-1)),
		stakingtypes.Infraction_INFRACTION_DOWNTIME,
	)
	expCommit := suite.commitSlashPacket(ctx, *packetData)

	// Miss 50 blocks and expect a slash packet to be sent
	for ; height <= missedBlockThreshold; height++ {
		ctx = ctx.WithBlockHeight(height)
		consumerSlashingKeeper.HandleValidatorSignature(ctx, vals[0].Address, valPower, comet.BlockIDFlagAbsent)
	}

	ctx = suite.consumerCtx()

	// check validator signing info
	res, _ := consumerSlashingKeeper.GetValidatorSigningInfo(ctx, consAddr)
	// expect increased jail time
	jailDuration, err := consumerSlashingKeeper.DowntimeJailDuration(ctx)
	suite.Require().NoError(err)
	suite.Require().True(res.JailedUntil.Equal(ctx.BlockTime().Add(jailDuration)), "did not update validator jailed until signing info")
	// expect missed block counters reset
	suite.Require().Zero(res.MissedBlocksCounter, "did not reset validator missed block counter")
	suite.Require().Zero(res.IndexOffset)
	consumerSlashingKeeper.IterateMissedBlockBitmap(ctx, consAddr, func(_ int64, missed bool) bool {
		suite.Require().True(missed)
		return false
	})

	// check that slash packet is queued
	pendingPackets := consumerKeeper.GetPendingPackets(ctx)
	suite.Require().NotEmpty(pendingPackets, "pending packets empty")
	suite.Require().Len(pendingPackets, 1, "pending packets len should be 1 is %d", len(pendingPackets))

	// clear queue, commit packets
	suite.consumerApp.GetConsumerKeeper().SendPackets(ctx)

	// Check slash record is created
	slashRecord, found := suite.consumerApp.GetConsumerKeeper().GetSlashRecord(suite.consumerCtx())
	suite.Require().True(found, "slash record not found")
	suite.Require().True(slashRecord.WaitingOnReply)
	suite.Require().Equal(slashRecord.SendTime, suite.consumerCtx().BlockTime())

	// check queue is not cleared, since no ack has been received from provider
	pendingPackets = suite.consumerApp.GetConsumerKeeper().GetPendingPackets(ctx)
	suite.Require().Len(pendingPackets, 1, "pending packets len should be 1 is %d", len(pendingPackets))

	// verify that the slash packet was sent
	gotCommit := consumerIBCKeeper.ChannelKeeper.GetPacketCommitment(ctx, ccv.ConsumerPortID, channelID, seq)
	suite.Require().NotNil(gotCommit, "did not found slash packet commitment")
	suite.Require().EqualValues(expCommit, gotCommit, "invalid slash packet commitment")

	// verify that the slash packet was sent
	suite.Require().True(consumerKeeper.OutstandingDowntime(ctx, consAddr))

	// check that the outstanding slashing flag prevents the jailed validator to keep missing block
	for ; height < missedBlockThreshold+signedBlocksWindow; height++ {
		ctx = ctx.WithBlockHeight(height)
		consumerSlashingKeeper.HandleValidatorSignature(ctx, vals[0].Address, valPower, comet.BlockIDFlagAbsent)
	}

	res, _ = consumerSlashingKeeper.GetValidatorSigningInfo(ctx, consAddr)

	suite.Require().Zero(res.MissedBlocksCounter, "did not reset validator missed block counter")
	suite.Require().Zero(res.IndexOffset)
	consumerSlashingKeeper.IterateMissedBlockBitmap(ctx, consAddr, func(_ int64, missed bool) bool {
		suite.Require().True(missed, "did not reset validator missed block bit array")
		return false
	})
}

// TestQueueAndSendSlashPacket tests the integration of QueueSlashPacket with SendPackets.
// In normal operation slash packets are queued in BeginBlock and sent in EndBlock.
// @Long Description@
// * Set up all CCV channels and then queue slash packets for both downtime and double-signing infractions.
// * Check that the correct number of slash requests are stored in the queue, including duplicates for downtime infractions.
// * Prepare the CCV channel for sending actual slash packets.
// * Send the slash packets and check that the outstanding downtime flags are correctly set for validators that were slashed
// for downtime infractions.
// * Ensure that the pending data packets queue is empty.
// TODO: Move to unit tests.
func (suite *CCVTestSuite) TestQueueAndSendSlashPacket() {
	suite.SetupCCVChannel(suite.path)

	consumerKeeper := suite.consumerApp.GetConsumerKeeper()
	consumerIBCKeeper := suite.consumerApp.GetIBCKeeper()

	ctx := suite.consumerChain.GetContext()
	channelID := suite.path.EndpointA.ChannelID

	// check that CCV channel isn't established
	_, ok := consumerKeeper.GetProviderChannel(ctx)
	suite.Require().False(ok)

	// expect to store 4 slash requests for downtime
	// and 4 slash request for double-signing
	type slashedVal struct {
		validator  abci.Validator
		infraction stakingtypes.Infraction
	}
	slashedVals := []slashedVal{}

	infraction := stakingtypes.Infraction_INFRACTION_DOWNTIME
	for j := 0; j < 2; j++ {
		for i := 0; i < 4; i++ {
			addr := ed25519.GenPrivKey().PubKey().Address()
			val := abci.Validator{
				Address: addr,
				Power:   int64(1),
			}
			consumerKeeper.QueueSlashPacket(ctx, val, 0, infraction)
			slashedVals = append(slashedVals, slashedVal{validator: val, infraction: infraction})
		}
		infraction = stakingtypes.Infraction_INFRACTION_DOUBLE_SIGN
	}

	// expect to store a duplicate for each slash request
	// in order to test the outstanding downtime logic
	for _, sv := range slashedVals {
		consumerKeeper.QueueSlashPacket(ctx, sv.validator, 0, sv.infraction)
	}

	// verify that all requests are stored except for
	// the downtime slash request duplicates
	dataPackets := consumerKeeper.GetPendingPackets(ctx)
	suite.Require().NotEmpty(dataPackets)
	suite.Require().Len(dataPackets, 12)

	// save consumer next sequence
	seq, _ := consumerIBCKeeper.ChannelKeeper.GetNextSequenceSend(ctx, ccv.ConsumerPortID, channelID)

	// establish ccv channel by sending an empty VSC packet to consumer endpoint
	suite.SendEmptyVSCPacket()

	// check that each pending data packet is sent once, as long as the prev slash packet was relayed/acked.
	// Note that consumer throttling blocks packet sending until a slash packet is successfully acked by the provider.
	for i := 0; i < 12; i++ {
		commit := consumerIBCKeeper.ChannelKeeper.GetPacketCommitment(ctx, ccv.ConsumerPortID, channelID, seq+uint64(i))
		suite.Require().NotNil(commit)
		relayAllCommittedPackets(suite, suite.consumerChain, suite.path, ccv.ConsumerPortID, channelID, 1)
	}

	// check that outstanding downtime flags
	// are all set to true for validators slashed for downtime requests
	for i := 0; i < 4; i++ {
		consAddr := sdk.ConsAddress(slashedVals[i].validator.Address)
		suite.Require().True(consumerKeeper.OutstandingDowntime(ctx, consAddr))
	}

	// SendPackets method should have already been called during
	// endblockers in relayAllCommittedPackets above

	// check that pending data packets got cleared
	dataPackets = consumerKeeper.GetPendingPackets(ctx)
	suite.Require().Empty(dataPackets)
	suite.Require().Len(dataPackets, 0)
}

// TestCISBeforeCCVEstablished tests that the consumer chain doesn't panic or
// have any undesired behavior when a slash packet is queued before the CCV channel is established.
// Then once the CCV channel is established, the slash packet should be sent soon after.
// @Long Description@
// * Check that no pending packets exist and that there's no slash record found.
// * Triggers a slashing event which queues a slash packet.
// * The slash packet should be queued but not sent, and it should stay like that until the CCV channel is established and the packet is sent.
// *Verify that a slashing record now exists, indicating that the slashing packet has been successfully sent.
func (suite *CCVTestSuite) TestCISBeforeCCVEstablished() {
	consumerKeeper := suite.consumerApp.GetConsumerKeeper()

	// Check pending packets is empty
	pendingPackets := consumerKeeper.GetPendingPackets(suite.consumerCtx())
	suite.Require().Len(pendingPackets, 0)

	// No slash record found (no slash sent)
	_, found := consumerKeeper.GetSlashRecord(suite.consumerCtx())
	suite.Require().False(found)

	consumerKeeper.SlashWithInfractionReason(suite.consumerCtx(), []byte{0x01, 0x02, 0x3},
		66, 4324, math.LegacyMustNewDecFromStr("0.05"), stakingtypes.Infraction_INFRACTION_DOWNTIME)

	// Check slash packet was queued
	pendingPackets = consumerKeeper.GetPendingPackets(suite.consumerCtx())
	suite.Require().Len(pendingPackets, 1)

	// but slash packet is not yet sent
	_, found = consumerKeeper.GetSlashRecord(suite.consumerCtx())
	suite.Require().False(found)

	// Pass 5 blocks, confirming the consumer doesn't panic
	for i := 0; i < 5; i++ {
		suite.consumerChain.NextBlock()
	}

	// Check packet is still queued
	pendingPackets = consumerKeeper.GetPendingPackets(suite.consumerCtx())
	suite.Require().Len(pendingPackets, 1)

	// establish ccv channel
	suite.SetupCCVChannel(suite.path)
	suite.SendEmptyVSCPacket()

	// Pass one more block, and confirm the packet is sent now that ccv channel is established
	suite.consumerChain.NextBlock()

	// Slash record should now be found (slash sent)
	_, found = consumerKeeper.GetSlashRecord(suite.consumerCtx())
	suite.Require().True(found)
}
