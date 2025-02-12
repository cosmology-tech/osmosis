package ibc_hooks

import (
	"encoding/json"
	"fmt"

	wasmkeeper "github.com/CosmWasm/wasmd/x/wasm/keeper"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	capabilitytypes "github.com/cosmos/cosmos-sdk/x/capability/types"
	"github.com/osmosis-labs/osmosis/v13/x/ibc-hooks/keeper"

	wasmtypes "github.com/CosmWasm/wasmd/x/wasm/types"

	"github.com/osmosis-labs/osmosis/v13/osmoutils"

	sdk "github.com/cosmos/cosmos-sdk/types"
	transfertypes "github.com/cosmos/ibc-go/v3/modules/apps/transfer/types"
	channeltypes "github.com/cosmos/ibc-go/v3/modules/core/04-channel/types"
	ibcexported "github.com/cosmos/ibc-go/v3/modules/core/exported"

	"github.com/osmosis-labs/osmosis/v13/x/ibc-hooks/types"
)

type ContractAck struct {
	ContractResult []byte `json:"contract_result"`
	IbcAck         []byte `json:"ibc_ack"`
}

type WasmHooks struct {
	ContractKeeper *wasmkeeper.PermissionedKeeper
	ibcHooksKeeper *keeper.Keeper
}

func NewWasmHooks(ibcHooksKeeper *keeper.Keeper, contractKeeper *wasmkeeper.PermissionedKeeper) WasmHooks {
	return WasmHooks{
		ContractKeeper: contractKeeper,
		ibcHooksKeeper: ibcHooksKeeper,
	}
}

func (h WasmHooks) ProperlyConfigured() bool {
	return h.ContractKeeper != nil && h.ibcHooksKeeper != nil
}

func (h WasmHooks) OnRecvPacketOverride(im IBCMiddleware, ctx sdk.Context, packet channeltypes.Packet, relayer sdk.AccAddress) ibcexported.Acknowledgement {
	if !h.ProperlyConfigured() {
		// Not configured
		return im.App.OnRecvPacket(ctx, packet, relayer)
	}

	isIcs20, data := isIcs20Packet(packet)
	if !isIcs20 {
		return im.App.OnRecvPacket(ctx, packet, relayer)
	}

	// Validate the memo
	isWasmRouted, contractAddr, msgBytes, err := ValidateAndParseMemo(data.GetMemo(), data.Receiver)
	if !isWasmRouted {
		return im.App.OnRecvPacket(ctx, packet, relayer)
	}
	if err != nil {
		return channeltypes.NewErrorAcknowledgement(err.Error())
	}
	if msgBytes == nil || contractAddr == nil { // This should never happen
		return channeltypes.NewErrorAcknowledgement("error in wasmhook message validation")
	}

	// The funds sent on this packet need to be transferred to the wasm hooks module address/
	// For this, we override the ICS20 packet's Receiver (essentially hijacking the funds for the module)
	// and execute the underlying OnRecvPacket() call (which should eventually land on the transfer app's
	// relay.go and send the sunds to the module.
	//
	// If that succeeds, we make the contract call
	data.Receiver = WasmHookModuleAccountAddr.String()
	bz, err := json.Marshal(data)
	if err != nil {
		return channeltypes.NewErrorAcknowledgement(fmt.Sprintf("cannot marshal the ICS20 packet: %s", err.Error()))
	}
	packet.Data = bz

	// Execute the receive
	ack := im.App.OnRecvPacket(ctx, packet, relayer)
	if !ack.Success() {
		return ack
	}

	amount, ok := sdk.NewIntFromString(data.GetAmount())
	if !ok {
		// This should never happen, as it should've been caught in the underlaying call to OnRecvPacket,
		// but returning here for completeness
		return channeltypes.NewErrorAcknowledgement("Invalid packet data: Amount is not an int")
	}

	// The packet's denom is the denom in the sender chain. This needs to be converted to the local denom.
	denom := osmoutils.MustExtractDenomFromPacketOnRecv(packet)
	funds := sdk.NewCoins(sdk.NewCoin(denom, amount))

	execMsg := wasmtypes.MsgExecuteContract{
		Sender:   WasmHookModuleAccountAddr.String(),
		Contract: contractAddr.String(),
		Msg:      msgBytes,
		Funds:    funds,
	}
	response, err := h.execWasmMsg(ctx, &execMsg)
	if err != nil {
		return channeltypes.NewErrorAcknowledgement(err.Error())
	}

	fullAck := ContractAck{ContractResult: response.Data, IbcAck: ack.Acknowledgement()}
	bz, err = json.Marshal(fullAck)
	if err != nil {
		return channeltypes.NewErrorAcknowledgement(fmt.Sprintf(types.ErrBadResponse, err.Error()))
	}

	return channeltypes.NewResultAcknowledgement(bz)
}

func (h WasmHooks) execWasmMsg(ctx sdk.Context, execMsg *wasmtypes.MsgExecuteContract) (*wasmtypes.MsgExecuteContractResponse, error) {
	if err := execMsg.ValidateBasic(); err != nil {
		return nil, fmt.Errorf(types.ErrBadExecutionMsg, err.Error())
	}
	wasmMsgServer := wasmkeeper.NewMsgServerImpl(h.ContractKeeper)
	return wasmMsgServer.ExecuteContract(sdk.WrapSDKContext(ctx), execMsg)
}

func isIcs20Packet(packet channeltypes.Packet) (isIcs20 bool, ics20data transfertypes.FungibleTokenPacketData) {
	var data transfertypes.FungibleTokenPacketData
	if err := json.Unmarshal(packet.GetData(), &data); err != nil {
		return false, data
	}
	return true, data
}

// jsonStringHasKey parses the memo as a json object and checks if it contains the key.
func jsonStringHasKey(memo, key string) (found bool, jsonObject map[string]interface{}) {
	jsonObject = make(map[string]interface{})

	// If there is no memo, the packet was either sent with an earlier version of IBC, or the memo was
	// intentionally left blank. Nothing to do here. Ignore the packet and pass it down the stack.
	if len(memo) == 0 {
		return false, jsonObject
	}

	// the jsonObject must be a valid JSON object
	err := json.Unmarshal([]byte(memo), &jsonObject)
	if err != nil {
		return false, jsonObject
	}

	// If the key doesn't exist, there's nothing to do on this hook. Continue by passing the packet
	// down the stack
	_, ok := jsonObject[key]
	if !ok {
		return false, jsonObject
	}

	return true, jsonObject
}

func ValidateAndParseMemo(memo string, receiver string) (isWasmRouted bool, contractAddr sdk.AccAddress, msgBytes []byte, err error) {
	isWasmRouted, metadata := jsonStringHasKey(memo, "wasm")
	if !isWasmRouted {
		return isWasmRouted, sdk.AccAddress{}, nil, nil
	}

	wasmRaw := metadata["wasm"]

	// Make sure the wasm key is a map. If it isn't, ignore this packet
	wasm, ok := wasmRaw.(map[string]interface{})
	if !ok {
		return isWasmRouted, sdk.AccAddress{}, nil,
			fmt.Errorf(types.ErrBadMetadataFormatMsg, memo, "wasm metadata is not a valid JSON map object")
	}

	// Get the contract
	contract, ok := wasm["contract"].(string)
	if !ok {
		// The tokens will be returned
		return isWasmRouted, sdk.AccAddress{}, nil,
			fmt.Errorf(types.ErrBadMetadataFormatMsg, memo, `Could not find key wasm["contract"]`)
	}

	contractAddr, err = sdk.AccAddressFromBech32(contract)
	if err != nil {
		return isWasmRouted, sdk.AccAddress{}, nil,
			fmt.Errorf(types.ErrBadMetadataFormatMsg, memo, `wasm["contract"] is not a valid bech32 address`)
	}

	// The contract and the receiver should be the same for the packet to be valid
	if contract != receiver {
		return isWasmRouted, sdk.AccAddress{}, nil,
			fmt.Errorf(types.ErrBadMetadataFormatMsg, memo, `wasm["contract"] should be the same as the receiver of the packet`)
	}

	// Ensure the message key is provided
	if wasm["msg"] == nil {
		return isWasmRouted, sdk.AccAddress{}, nil,
			fmt.Errorf(types.ErrBadMetadataFormatMsg, memo, `Could not find key wasm["msg"]`)
	}

	// Make sure the msg key is a map. If it isn't, return an error
	_, ok = wasm["msg"].(map[string]interface{})
	if !ok {
		return isWasmRouted, sdk.AccAddress{}, nil,
			fmt.Errorf(types.ErrBadMetadataFormatMsg, memo, `wasm["msg"] is not a map object`)
	}

	// Get the message string by serializing the map
	msgBytes, err = json.Marshal(wasm["msg"])
	if err != nil {
		// The tokens will be returned
		return isWasmRouted, sdk.AccAddress{}, nil,
			fmt.Errorf(types.ErrBadMetadataFormatMsg, memo, err.Error())
	}

	return isWasmRouted, contractAddr, msgBytes, nil
}

func (h WasmHooks) SendPacketOverride(i ICS4Middleware, ctx sdk.Context, chanCap *capabilitytypes.Capability, packet ibcexported.PacketI) error {
	concretePacket, ok := packet.(channeltypes.Packet)
	if !ok {
		return i.channel.SendPacket(ctx, chanCap, packet) // continue
	}

	isIcs20, data := isIcs20Packet(concretePacket)
	if !isIcs20 {
		return i.channel.SendPacket(ctx, chanCap, packet) // continue
	}

	isCallbackRouted, metadata := jsonStringHasKey(data.GetMemo(), types.IBCCallbackKey)
	if !isCallbackRouted {
		return i.channel.SendPacket(ctx, chanCap, packet) // continue
	}

	// We remove the callback metadata from the memo as it has already been processed.

	// If the only available key in the memo is the callback, we should remove the memo
	// from the data completely so the packet is sent without it.
	// This way receiver chains that are on old versions of IBC will be able to process the packet

	callbackRaw := metadata[types.IBCCallbackKey] // This will be used later.
	delete(metadata, types.IBCCallbackKey)
	bzMetadata, err := json.Marshal(metadata)
	if err != nil {
		return sdkerrors.Wrap(err, "Send packet with callback error")
	}
	stringMetadata := string(bzMetadata)
	if stringMetadata == "{}" {
		data.Memo = ""
	} else {
		data.Memo = stringMetadata
	}
	dataBytes, err := json.Marshal(data)
	if err != nil {
		return sdkerrors.Wrap(err, "Send packet with callback error")
	}

	packetWithoutCallbackMemo := channeltypes.Packet{
		Sequence:           concretePacket.Sequence,
		SourcePort:         concretePacket.SourcePort,
		SourceChannel:      concretePacket.SourceChannel,
		DestinationPort:    concretePacket.DestinationPort,
		DestinationChannel: concretePacket.DestinationChannel,
		Data:               dataBytes,
		TimeoutTimestamp:   concretePacket.TimeoutTimestamp,
		TimeoutHeight:      concretePacket.TimeoutHeight,
	}

	err = i.channel.SendPacket(ctx, chanCap, packetWithoutCallbackMemo)
	if err != nil {
		return err
	}

	// Make sure the callback contract is a string and a valid bech32 addr. If it isn't, ignore this packet
	contract, ok := callbackRaw.(string)
	if !ok {
		return nil
	}
	_, err = sdk.AccAddressFromBech32(contract)
	if err != nil {
		return nil
	}

	h.ibcHooksKeeper.StorePacketCallback(ctx, packet.GetSourceChannel(), packet.GetSequence(), contract)
	return nil
}

func (h WasmHooks) OnAcknowledgementPacketOverride(im IBCMiddleware, ctx sdk.Context, packet channeltypes.Packet, acknowledgement []byte, relayer sdk.AccAddress) error {
	err := im.App.OnAcknowledgementPacket(ctx, packet, acknowledgement, relayer)
	if err != nil {
		return err
	}

	if !h.ProperlyConfigured() {
		// Not configured. Return from the underlying implementation
		return nil
	}

	contract := h.ibcHooksKeeper.GetPacketCallback(ctx, packet.GetSourceChannel(), packet.GetSequence())
	if contract == "" {
		// No callback configured
		return nil
	}

	contractAddr, err := sdk.AccAddressFromBech32(contract)
	if err != nil {
		return sdkerrors.Wrap(err, "Ack callback error") // The callback configured is not a beck32. Error out
	}

	success := "false"
	if !osmoutils.IsAckError(acknowledgement) {
		success = "true"
	}

	// Notify the sender that the ack has been received
	ackAsJson, err := json.Marshal(acknowledgement)
	if err != nil {
		// If the ack is not a json object, error
		return err
	}

	sudoMsg := []byte(fmt.Sprintf(
		`{"receive_ack": {"channel": "%s", "sequence": %d, "ack": %s, "success": %s}}`,
		packet.SourceChannel, packet.Sequence, ackAsJson, success))
	_, err = h.ContractKeeper.Sudo(ctx, contractAddr, sudoMsg)
	if err != nil {
		// error processing the callback
		return sdkerrors.Wrap(err, "Ack callback error")
	}
	h.ibcHooksKeeper.DeletePacketCallback(ctx, packet.GetSourceChannel(), packet.GetSequence())
	return nil
}
