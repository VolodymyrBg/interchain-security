package integration

import (
	"time"

	ibctmtypes "github.com/cosmos/ibc-go/v10/modules/light-clients/07-tendermint"

	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"

	tmtypes "github.com/cometbft/cometbft/types"

	testutil "github.com/cosmos/interchain-security/v7/testutil/crypto"
	"github.com/cosmos/interchain-security/v7/x/ccv/provider/types"
)

// TestHandleConsumerMisbehaviour tests the handling of consumer misbehavior.
// @Long Description@
// * Set up a CCV channel and send an empty VSC packet to ensure that the consumer client revision height is greater than 0.
// * Construct a Misbehaviour object with two conflicting headers and process the equivocation evidence.
// * Verify that the provider chain correctly processes this misbehavior.
// * Ensure that all involved validators are jailed, tombstoned, and slashed according to the expected outcomes.
// * Assert that their tokens are adjusted based on the slashing fraction.
func (s *CCVTestSuite) TestHandleConsumerMisbehaviour() {
	s.SetupCCVChannel(s.path)
	// required to have the consumer client revision height greater than 0
	s.SendEmptyVSCPacket()

	for _, v := range s.providerChain.Vals.Validators {
		s.setDefaultValSigningInfo(*v)
	}

	altTime := s.providerCtx().BlockTime().Add(time.Minute)

	clientHeight := s.consumerChain.LatestCommittedHeader.TrustedHeight
	clientTMValset := tmtypes.NewValidatorSet(s.consumerChain.Vals.Validators)
	clientSigners := s.consumerChain.Signers

	misb := &ibctmtypes.Misbehaviour{
		ClientId: s.path.EndpointA.ClientID,
		Header1: s.consumerChain.CreateTMClientHeader(
			s.getFirstBundle().Chain.ChainID,
			int64(clientHeight.RevisionHeight+1),
			clientHeight,
			altTime,
			clientTMValset,
			clientTMValset,
			clientTMValset,
			clientSigners,
		),
		// create a different header by changing the header timestamp only
		// in order to create an equivocation, i.e. both headers have the same deterministic states
		Header2: s.consumerChain.CreateTMClientHeader(
			s.getFirstBundle().Chain.ChainID,
			int64(clientHeight.RevisionHeight+1),
			clientHeight,
			altTime.Add(10*time.Second),
			clientTMValset,
			clientTMValset,
			clientTMValset,
			clientSigners,
		),
	}

	// we assume that all validators have the same number of initial tokens
	validator, _ := s.getValByIdx(0)
	initialTokens := math.LegacyNewDecFromInt(validator.GetTokens())

	err := s.providerApp.GetProviderKeeper().HandleConsumerMisbehaviour(s.providerCtx(), s.getFirstBundle().ConsumerId, *misb)
	s.NoError(err)

	// verify that validators are jailed, tombstoned, and slashed
	for _, v := range clientTMValset.Validators {
		consuAddr := sdk.ConsAddress(v.Address.Bytes())
		provAddr := s.providerApp.GetProviderKeeper().GetProviderAddrFromConsumerAddr(s.providerCtx(), s.getFirstBundle().ConsumerId, types.NewConsumerConsAddress(consuAddr))
		val, err := s.providerApp.GetTestStakingKeeper().GetValidatorByConsAddr(s.providerCtx(), provAddr.Address)
		s.Require().NoError(err)
		s.Require().True(val.Jailed)
		s.Require().True(s.providerApp.GetTestSlashingKeeper().IsTombstoned(s.providerCtx(), provAddr.ToSdkConsAddr()))

		validator, _ := s.providerApp.GetTestStakingKeeper().GetValidator(s.providerCtx(), provAddr.ToSdkConsAddr().Bytes())
		infractionParam, err := s.providerApp.GetProviderKeeper().GetInfractionParameters(s.providerCtx(), s.getFirstBundle().ConsumerId)
		s.Require().NoError(err)
		slashFraction := infractionParam.DoubleSign.SlashFraction
		actualTokens := math.LegacyNewDecFromInt(validator.GetTokens())
		s.Require().True(initialTokens.Sub(initialTokens.Mul(slashFraction)).Equal(actualTokens))
	}
}

// TestGetByzantineValidators checks the GetByzantineValidators function on various instances of misbehaviour.
// @Long Description@
// * Set up a provider and consumer chain.
// * Create a header with a subset of the validators on the consumer chain, then create a second header (in a variety of different ways),
// and check which validators are considered Byzantine by calling the GetByzantineValidators function.
// * The test scenarios are:
// - when one of the headers is empty, the function should return an error
// - when one of the headers has a corrupted validator set (e.g. by a validator having a different public key), the function should return an error
// - when the signatures in one of the headers are corrupted, the function should return an error
// - when the attack is an amnesia attack (i.e. the headers have different block IDs), no validator is considered byzantine
// - for non-amnesia misbehaviour, all validators that signed both headers are considered byzantine
func (s *CCVTestSuite) TestGetByzantineValidators() {
	s.SetupCCVChannel(s.path)
	// required to have the consumer client revision height greater than 0
	s.SendEmptyVSCPacket()

	altTime := s.providerCtx().BlockTime().Add(time.Minute)

	// Get the consumer client validator set
	clientHeight := s.consumerChain.LatestCommittedHeader.TrustedHeight
	clientTMValset := tmtypes.NewValidatorSet(s.consumerChain.Vals.Validators)
	clientSigners := s.consumerChain.Signers

	// Create a subset of the consumer client validator set
	altValset := tmtypes.NewValidatorSet(s.consumerChain.Vals.Validators[0:3])
	altSigners := make(map[string]tmtypes.PrivValidator, 3)
	altSigners[clientTMValset.Validators[0].Address.String()] = clientSigners[clientTMValset.Validators[0].Address.String()]
	altSigners[clientTMValset.Validators[1].Address.String()] = clientSigners[clientTMValset.Validators[1].Address.String()]
	altSigners[clientTMValset.Validators[2].Address.String()] = clientSigners[clientTMValset.Validators[2].Address.String()]

	// create a consumer client header
	clientHeader := s.consumerChain.CreateTMClientHeader(
		s.getFirstBundle().Chain.ChainID,
		int64(clientHeight.RevisionHeight+1),
		clientHeight,
		altTime,
		clientTMValset,
		clientTMValset,
		clientTMValset,
		clientSigners,
	)

	testCases := []struct {
		name                   string
		getMisbehaviour        func() *ibctmtypes.Misbehaviour
		expByzantineValidators []*tmtypes.Validator
		expPass                bool
	}{
		{
			"invalid misbehaviour - Header1 is empty",
			func() *ibctmtypes.Misbehaviour {
				return &ibctmtypes.Misbehaviour{
					Header1: &ibctmtypes.Header{},
					Header2: clientHeader,
				}
			},
			nil,
			false,
		},
		{
			"invalid headers - Header2 is empty",
			func() *ibctmtypes.Misbehaviour {
				return &ibctmtypes.Misbehaviour{
					Header1: clientHeader,
					Header2: &ibctmtypes.Header{},
				}
			},
			nil,
			false,
		},
		{
			"incorrect valset - shouldn't pass",
			func() *ibctmtypes.Misbehaviour {
				clientHeader := s.consumerChain.CreateTMClientHeader(
					s.getFirstBundle().Chain.ChainID,
					int64(clientHeight.RevisionHeight+1),
					clientHeight,
					altTime.Add(time.Minute),
					clientTMValset,
					clientTMValset,
					clientTMValset,
					clientSigners,
				)

				clientHeaderWithCorruptedValset := s.consumerChain.CreateTMClientHeader(
					s.getFirstBundle().Chain.ChainID,
					int64(clientHeight.RevisionHeight+1),
					clientHeight,
					altTime.Add(time.Hour),
					clientTMValset,
					clientTMValset,
					clientTMValset,
					clientSigners,
				)

				// change a validator public key in one the second header
				testutil.CorruptValidatorPubkeyInHeader(clientHeaderWithCorruptedValset, clientTMValset.Validators[0].Address)

				return &ibctmtypes.Misbehaviour{
					ClientId: s.path.EndpointA.ClientID,
					Header1:  clientHeader,
					Header2:  clientHeaderWithCorruptedValset,
				}
			},
			[]*tmtypes.Validator{},
			false,
		},
		{
			"incorrect valset 2 - shouldn't pass",
			func() *ibctmtypes.Misbehaviour {
				clientHeader := s.consumerChain.CreateTMClientHeader(
					s.getFirstBundle().Chain.ChainID,
					int64(clientHeight.RevisionHeight+1),
					clientHeight,
					altTime.Add(time.Minute),
					clientTMValset,
					clientTMValset,
					clientTMValset,
					clientSigners,
				)

				clientHeaderWithCorruptedSigs := s.consumerChain.CreateTMClientHeader(
					s.getFirstBundle().Chain.ChainID,
					int64(clientHeight.RevisionHeight+1),
					clientHeight,
					altTime.Add(time.Hour),
					clientTMValset,
					clientTMValset,
					clientTMValset,
					clientSigners,
				)

				// change the valset in the header
				vs, _ := altValset.ToProto()
				clientHeader.ValidatorSet.Validators = vs.Validators[:3]
				clientHeaderWithCorruptedSigs.ValidatorSet.Validators = vs.Validators[:3]

				return &ibctmtypes.Misbehaviour{
					ClientId: s.path.EndpointA.ClientID,
					Header1:  clientHeader,
					Header2:  clientHeaderWithCorruptedSigs,
				}
			},
			[]*tmtypes.Validator{},
			false,
		},
		{
			"incorrect signatures - shouldn't pass",
			func() *ibctmtypes.Misbehaviour {
				clientHeader := s.consumerChain.CreateTMClientHeader(
					s.getFirstBundle().Chain.ChainID,
					int64(clientHeight.RevisionHeight+1),
					clientHeight,
					altTime.Add(time.Minute),
					clientTMValset,
					clientTMValset,
					clientTMValset,
					clientSigners,
				)

				clientHeaderWithCorruptedSigs := s.consumerChain.CreateTMClientHeader(
					s.getFirstBundle().Chain.ChainID,
					int64(clientHeight.RevisionHeight+1),
					clientHeight,
					altTime.Add(time.Hour),
					clientTMValset,
					clientTMValset,
					clientTMValset,
					clientSigners,
				)

				// change the signature of one of the validator in the header
				testutil.CorruptCommitSigsInHeader(clientHeaderWithCorruptedSigs, clientTMValset.Validators[0].Address)

				return &ibctmtypes.Misbehaviour{
					ClientId: s.path.EndpointA.ClientID,
					Header1:  clientHeader,
					Header2:  clientHeaderWithCorruptedSigs,
				}
			},
			[]*tmtypes.Validator{},
			false,
		},
		{
			"light client attack - lunatic attack",
			func() *ibctmtypes.Misbehaviour {
				return &ibctmtypes.Misbehaviour{
					ClientId: s.path.EndpointA.ClientID,
					Header1:  clientHeader,
					// the resulting header contains invalid fields
					// i.e. ValidatorsHash, NextValidatorsHash.
					Header2: s.consumerChain.CreateTMClientHeader(
						s.getFirstBundle().Chain.ChainID,
						int64(clientHeight.RevisionHeight+1),
						clientHeight,
						altTime,
						altValset,
						altValset,
						clientTMValset,
						altSigners,
					),
				}
			},
			// Expect to get only the validators
			// who signed both headers
			altValset.Validators,
			true,
		},
		{
			"light client attack - equivocation",
			func() *ibctmtypes.Misbehaviour {
				return &ibctmtypes.Misbehaviour{
					ClientId: s.path.EndpointA.ClientID,
					Header1:  clientHeader,
					// the resulting header contains a different BlockID
					Header2: s.consumerChain.CreateTMClientHeader(
						s.getFirstBundle().Chain.ChainID,
						int64(clientHeight.RevisionHeight+1),
						clientHeight,
						altTime.Add(time.Minute),
						clientTMValset,
						clientTMValset,
						clientTMValset,
						clientSigners,
					),
				}
			},
			// Expect to get the entire valset since
			// all validators double-signed
			clientTMValset.Validators,
			true,
		},
		{
			"light client attack - amnesia",
			func() *ibctmtypes.Misbehaviour {
				// create a valid header with a different hash
				// and commit round
				amnesiaHeader := s.consumerChain.CreateTMClientHeader(
					s.getFirstBundle().Chain.ChainID,
					int64(clientHeight.RevisionHeight+1),
					clientHeight,
					altTime.Add(time.Minute),
					clientTMValset,
					clientTMValset,
					clientTMValset,
					clientSigners,
				)
				amnesiaHeader.Commit.Round = 2

				return &ibctmtypes.Misbehaviour{
					ClientId: s.path.EndpointA.ClientID,
					Header1:  clientHeader,
					Header2:  amnesiaHeader,
				}
			},
			// Expect no validators
			// since amnesia attacks are dropped
			[]*tmtypes.Validator{},
			true,
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			byzantineValidators, err := s.providerApp.GetProviderKeeper().GetByzantineValidators(
				s.providerCtx(),
				*tc.getMisbehaviour(),
			)
			if tc.expPass {
				s.NoError(err)
				s.Equal(len(tc.expByzantineValidators), len(byzantineValidators))

				// For both lunatic and equivocation attacks, all the validators
				// who signed both headers
				if len(tc.expByzantineValidators) > 0 {
					equivocatingVals := tc.getMisbehaviour().Header2.ValidatorSet
					s.Equal(len(equivocatingVals.Validators), len(byzantineValidators))

					vs, err := tmtypes.ValidatorSetFromProto(equivocatingVals)
					s.NoError(err)

					for _, v := range tc.expByzantineValidators {
						idx, _ := vs.GetByAddress(v.Address)
						s.True(idx >= 0)
					}
				}
			} else {
				s.Error(err)
			}
		})
	}
}

// TestCheckMisbehaviour tests that the CheckMisbehaviour function correctly checks for misbehaviour.
// @Long Description@
// * Set up a provider and consumer chain.
// * Create a valid client header and then create a misbehaviour by creating a second header in a variety of different ways.
// * Check that the CheckMisbehaviour function correctly checks for misbehaviour by verifying that
// it returns an error when the misbehaviour is invalid and no error when the misbehaviour is valid.
// * The test scenarios are:
//   - both headers are identical (returns an error)
//   - the misbehaviour is not for the consumer chain (returns an error)
//   - passing an invalid client id (returns an error)
//   - passing a misbehaviour with different header height (returns an error)
//   - passing a misbehaviour older than the min equivocation evidence height (returns an error)
//   - one header of the misbehaviour has insufficient voting power (returns an error)
//   - passing a valid misbehaviour (no error)
//
// * Test does not test actually submitting the misbehaviour to the chain or freezing the client.
func (s *CCVTestSuite) TestCheckMisbehaviour() {
	s.SetupCCVChannel(s.path)
	// required to have the consumer client revision height greater than 0
	s.SendEmptyVSCPacket()

	// create signing info for all validators
	for _, v := range s.providerChain.Vals.Validators {
		s.setDefaultValSigningInfo(*v)
	}

	// create a new header timestamp
	headerTs := s.providerCtx().BlockTime().Add(time.Minute)

	// get trusted validators and height
	clientHeight := s.consumerChain.LatestCommittedHeader.TrustedHeight
	clientTMValset := tmtypes.NewValidatorSet(s.consumerChain.Vals.Validators)
	clientSigners := s.consumerChain.Signers

	// create a valid client header
	clientHeader := s.consumerChain.CreateTMClientHeader(
		s.getFirstBundle().Chain.ChainID,
		int64(clientHeight.RevisionHeight+1),
		clientHeight,
		headerTs,
		clientTMValset,
		clientTMValset,
		clientTMValset,
		clientSigners,
	)

	// create an alternative validator set using more than 1/3 of the trusted validator set
	altValset := tmtypes.NewValidatorSet(s.consumerChain.Vals.Validators[0:2])
	altSigners := make(map[string]tmtypes.PrivValidator, 2)
	altSigners[clientTMValset.Validators[0].Address.String()] = clientSigners[clientTMValset.Validators[0].Address.String()]
	altSigners[clientTMValset.Validators[1].Address.String()] = clientSigners[clientTMValset.Validators[1].Address.String()]

	// create a conflicting client with different block ID using
	// to alternative validator set
	clientHeaderWithDiffBlockID := s.consumerChain.CreateTMClientHeader(
		s.getFirstBundle().Chain.ChainID,
		int64(clientHeight.RevisionHeight+1),
		clientHeight,
		headerTs,
		altValset,
		altValset,
		clientTMValset, // trusted valset stays the same
		altSigners,
	)

	// create an alternative validator set using less than 1/3 of the trusted validator set
	altValset2 := tmtypes.NewValidatorSet(s.consumerChain.Vals.Validators[0:1])
	altSigners2 := make(map[string]tmtypes.PrivValidator, 1)
	altSigners2[clientTMValset.Validators[0].Address.String()] = clientSigners[clientTMValset.Validators[0].Address.String()]

	// create a conflicting client header with insufficient voting power
	clientHeaderWithInsufficientVotingPower := s.consumerChain.CreateTMClientHeader(
		s.getFirstBundle().Chain.ChainID,
		int64(clientHeight.RevisionHeight+1),
		clientHeight,
		// use a different block time to change the header BlockID
		headerTs.Add(time.Hour),
		altValset2,
		altValset2,
		clientTMValset,
		altSigners2,
	)

	// Set the equivocation evidence min height to the previous block height
	equivocationEvidenceMinHeight := clientHeight.RevisionHeight + 1
	s.providerApp.GetProviderKeeper().SetEquivocationEvidenceMinHeight(
		s.providerCtx(),
		s.getFirstBundle().ConsumerId,
		equivocationEvidenceMinHeight,
	)

	testCases := []struct {
		name         string
		misbehaviour *ibctmtypes.Misbehaviour
		expPass      bool
	}{
		{
			"identical headers - shouldn't pass",
			&ibctmtypes.Misbehaviour{
				ClientId: s.path.EndpointA.ClientID,
				Header1:  clientHeader,
				Header2:  clientHeader,
			},
			false,
		},
		{
			"misbehaviour isn't for a consumer chain - shouldn't pass",
			&ibctmtypes.Misbehaviour{
				ClientId: s.path.EndpointA.ClientID,
				Header1: s.consumerChain.CreateTMClientHeader(
					"aChainID",
					int64(clientHeight.RevisionHeight+1),
					clientHeight,
					headerTs,
					altValset,
					altValset,
					clientTMValset,
					altSigners,
				),
				Header2: clientHeader,
			},
			false,
		},
		{
			"client ID doesn't correspond to the client ID of consumer chain  - shouldn't pass",
			&ibctmtypes.Misbehaviour{
				ClientId: "clientID",
				Header1:  clientHeader,
				Header2:  clientHeaderWithDiffBlockID,
			},
			false,
		},
		{
			"invalid misbehaviour with different header height  - shouldn't pass",
			&ibctmtypes.Misbehaviour{
				ClientId: s.path.EndpointA.ClientID,
				Header1:  clientHeader,
				Header2: s.consumerChain.CreateTMClientHeader(
					s.getFirstBundle().Chain.ChainID,
					int64(clientHeight.RevisionHeight+2),
					clientHeight,
					headerTs,
					altValset,
					altValset,
					clientTMValset,
					altSigners,
				),
			},
			false,
		},
		{
			"invalid misbehaviour older than the min equivocation evidence height - shouldn't pass",
			&ibctmtypes.Misbehaviour{
				ClientId: s.path.EndpointA.ClientID,
				Header1: s.consumerChain.CreateTMClientHeader(
					s.getFirstBundle().Chain.ChainID,
					int64(equivocationEvidenceMinHeight-1),
					clientHeight,
					headerTs,
					altValset,
					altValset,
					clientTMValset,
					altSigners,
				),
				Header2: s.consumerChain.CreateTMClientHeader(
					s.getFirstBundle().Chain.ChainID,
					int64(equivocationEvidenceMinHeight-1),
					clientHeight,
					headerTs,
					clientTMValset,
					clientTMValset,
					clientTMValset,
					clientSigners,
				),
			},
			false,
		},
		{
			"one header of the misbehaviour has insufficient voting power - shouldn't pass",
			&ibctmtypes.Misbehaviour{
				ClientId: s.path.EndpointA.ClientID,
				Header1:  clientHeader,
				Header2:  clientHeaderWithInsufficientVotingPower,
			},
			false,
		},
		{
			"valid misbehaviour - should pass",
			&ibctmtypes.Misbehaviour{
				ClientId: s.path.EndpointA.ClientID,
				Header1:  clientHeader,
				// create header using a different validator set
				Header2: clientHeaderWithDiffBlockID,
			},
			true,
		},
	}

	for _, tc := range testCases {
		s.Run(tc.name, func() {
			err := s.providerApp.GetProviderKeeper().CheckMisbehaviour(s.providerCtx(), s.getFirstBundle().ConsumerId, *tc.misbehaviour)
			cs, ok := s.providerApp.GetIBCKeeper().ClientKeeper.GetClientState(s.providerCtx(), s.path.EndpointA.ClientID)
			s.Require().True(ok)
			// verify that the client wasn't frozen
			s.Require().Zero(cs.(*ibctmtypes.ClientState).FrozenHeight)
			if tc.expPass {
				s.NoError(err)
			} else {
				s.Error(err)
			}
		})
	}
}
