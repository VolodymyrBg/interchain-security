package types

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"time"

	clienttypes "github.com/cosmos/ibc-go/v10/modules/core/02-client/types"
	channeltypes "github.com/cosmos/ibc-go/v10/modules/core/04-channel/types"

	errorsmod "cosmossdk.io/errors"

	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/bech32"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	abci "github.com/cometbft/cometbft/abci/types"
	tmprotocrypto "github.com/cometbft/cometbft/proto/tendermint/crypto"
)

func AccumulateChanges(currentChanges, newChanges []abci.ValidatorUpdate) []abci.ValidatorUpdate {
	m := make(map[string]abci.ValidatorUpdate)

	for i := 0; i < len(currentChanges); i++ {
		m[currentChanges[i].PubKey.String()] = currentChanges[i]
	}

	for i := 0; i < len(newChanges); i++ {
		m[newChanges[i].PubKey.String()] = newChanges[i]
	}

	var out []abci.ValidatorUpdate

	for _, update := range m {
		out = append(out, update)
	}

	// The list of tendermint updates should hash the same across all consensus nodes
	// that means it is necessary to sort for determinism.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Power != out[j].Power {
			return out[i].Power > out[j].Power
		}
		return out[i].PubKey.String() > out[j].PubKey.String()
	})

	return out
}

// TMCryptoPublicKeyToConsAddr converts a TM public key to an SDK public key
// and returns the associated consensus address
func TMCryptoPublicKeyToConsAddr(k tmprotocrypto.PublicKey) (sdk.ConsAddress, error) {
	sdkK, err := cryptocodec.FromCmtProtoPublicKey(k)
	if err != nil {
		return nil, err
	}
	return sdk.GetConsAddress(sdkK), nil
}

// SendIBCPacket sends an IBC packet with packetData
// over the source channelID and portID
func SendIBCPacket(
	ctx sdk.Context,
	channelKeeper ChannelKeeper,
	sourceChannelID string,
	sourcePortID string,
	packetData []byte,
	timeoutPeriod time.Duration,
) error {
	_, ok := channelKeeper.GetChannel(ctx, sourcePortID, sourceChannelID)
	if !ok {
		return errorsmod.Wrapf(channeltypes.ErrChannelNotFound, "channel not found for channel ID: %s", sourceChannelID)
	}

	_, err := channelKeeper.SendPacket(ctx,
		sourcePortID,
		sourceChannelID,
		clienttypes.Height{}, //  timeout height disabled
		uint64(ctx.BlockTime().Add(timeoutPeriod).UnixNano()), // timeout timestamp
		packetData,
	)
	return err
}

func NewErrorAcknowledgementWithLog(ctx sdk.Context, err error) channeltypes.Acknowledgement {
	ctx.Logger().Error("IBC ErrorAcknowledgement constructed", "error", err)
	return channeltypes.NewErrorAcknowledgement(err)
}

// AppendMany appends a variable number of byte slices together
func AppendMany(byteses ...[]byte) (out []byte) {
	for _, bytes := range byteses {
		out = append(out, bytes...)
	}
	return out
}

func PanicIfZeroOrNil(x interface{}, nameForPanicMsg string) {
	if x == nil || reflect.ValueOf(x).IsZero() {
		panic("zero or nil value for " + nameForPanicMsg)
	}
}

// GetConsAddrFromBech32 returns a ConsAddress from a Bech32 with an arbitrary prefix
func GetConsAddrFromBech32(bech32str string) (sdk.ConsAddress, error) {
	bech32Addr := strings.TrimSpace(bech32str)
	if len(bech32Addr) == 0 {
		return nil, errors.New("couldn't parse empty input")
	}
	// remove bech32 prefix
	_, addr, err := bech32.DecodeAndConvert(bech32Addr)
	if err != nil {
		return nil, errors.New("couldn't find valid bech32")
	}
	return sdk.ConsAddress(addr), nil
}

// GetLastBondedValidatorsUtil iterates the last validator powers in the staking module
// and returns the first maxVals many validators with the largest powers.
func GetLastBondedValidatorsUtil(ctx sdk.Context, stakingKeeper StakingKeeper, maxVals uint32) ([]stakingtypes.Validator, error) {
	// get the bonded validators from the staking module, sorted by power
	bondedValidators, err := stakingKeeper.GetBondedValidatorsByPower(ctx)
	if err != nil {
		return nil, err
	}

	// get the first maxVals many validators
	if uint32(len(bondedValidators)) < maxVals {
		return bondedValidators, nil // no need to truncate
	}

	bondedValidators = bondedValidators[:maxVals]

	return bondedValidators, nil
}
