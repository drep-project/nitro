//
// Copyright 2021, Offchain Labs, Inc. All rights reserved.
//

package precompiles

import (
	"log"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"unicode"

	templates "github.com/offchainlabs/arbstate/solgen/go"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
)

type addr = common.Address
type mech = *vm.EVM
type huge = *big.Int

type ArbosPrecompile interface {
	GasToCharge(input []byte) uint64

	// Important fields: evm.StateDB and evm.Config.Tracer
	// NOTE: if precompileAddress != actingAsAddress, watch out!
	// This is a delegatecall or callcode, so caller might be wrong.
	// In that case, unless this precompile is pure, it should probably revert.
	Call(
		input []byte,
		precompileAddress common.Address,
		actingAsAddress common.Address,
		caller common.Address,
		value *big.Int,
		readOnly bool,
		evm *vm.EVM,
	) (output []byte, err error)

	Precompile() Precompile
}

type purity uint8

const (
	pure purity = iota
	view
	write
	payable
)

type Precompile struct {
	methods map[[4]byte]PrecompileMethod
	events  map[string]PrecompileEvent
}

type PrecompileMethod struct {
	name        string
	template    abi.Method
	purity      purity
	handler     reflect.Method
	gascost     reflect.Method
	implementer reflect.Value
}

type PrecompileEvent struct {
	name     string
	template abi.Event
}

// Make a precompile for the given hardhat-to-geth bindings, ensuring that the implementer
// supports each method.
func makePrecompile(metadata *bind.MetaData, implementer interface{}) (addr, ArbosPrecompile) {
	source, err := abi.JSON(strings.NewReader(metadata.ABI))
	if err != nil {
		log.Fatal("Bad ABI")
	}

	implementerType := reflect.TypeOf(implementer)
	contract := implementerType.Elem().Name()

	_, ok := implementerType.Elem().FieldByName("Address")
	if !ok {
		log.Fatal("Implementer for precompile ", contract, " is missing an Address field")
	}

	address, ok := reflect.ValueOf(implementer).Elem().FieldByName("Address").Interface().(addr)
	if !ok {
		log.Fatal("Implementer for precompile ", contract, "'s Address field has the wrong type")
	}

	methods := make(map[[4]byte]PrecompileMethod)
	events := make(map[string]PrecompileEvent)

	for _, method := range source.Methods {

		name := method.RawName
		capitalize := string(unicode.ToUpper(rune(name[0])))
		name = capitalize + name[1:]

		if len(method.ID) != 4 {
			log.Fatal("Method ID isn't 4 bytes")
		}
		id := *(*[4]byte)(method.ID)

		// check that the implementer has a supporting implementation for this method

		handler, ok := implementerType.MethodByName(name)
		if !ok {
			log.Fatal("Precompile ", contract, " must implement ", name)
		}

		var needs = []reflect.Type{
			implementerType,                  // the contract itself
			reflect.TypeOf(common.Address{}), // the method's caller
		}

		var purity purity

		switch method.StateMutability {
		case "pure":
			purity = pure
		case "view":
			needs = append(needs, reflect.TypeOf(&vm.EVM{}))
			purity = view
		case "nonpayable":
			needs = append(needs, reflect.TypeOf(&vm.EVM{}))
			purity = write
		case "payable":
			needs = append(needs, reflect.TypeOf(&vm.EVM{}))
			needs = append(needs, reflect.TypeOf(&big.Int{}))
			purity = payable
		default:
			log.Fatal("Unknown state mutability ", method.StateMutability)
		}

		for _, arg := range method.Inputs {
			needs = append(needs, arg.Type.GetType())
		}

		var outputs = []reflect.Type{}
		for _, out := range method.Outputs {
			outputs = append(outputs, out.Type.GetType())
		}
		outputs = append(outputs, reflect.TypeOf((*error)(nil)).Elem())

		expectedHandlerType := reflect.FuncOf(needs, outputs, false)

		if handler.Type != expectedHandlerType {
			log.Fatal(
				"Precompile "+contract+"'s "+name+"'s implementer has the wrong type\n",
				"\texpected:\t", expectedHandlerType, "\n\tbut have:\t", handler.Type,
			)
		}

		// ensure we have a matching gascost func
		gascost, ok := implementerType.MethodByName(name + "GasCost")
		if !ok {
			log.Fatal("Precompile ", contract, " must implement ", name+"GasCost")
		}

		needs = []reflect.Type{
			implementerType, // the contract itself
		}
		for _, arg := range method.Inputs {
			needs = append(needs, arg.Type.GetType())
		}

		uint64Type := []reflect.Type{reflect.TypeOf(uint64(0))}
		expectedGasCostType := reflect.FuncOf(needs, uint64Type, false)

		if gascost.Type != expectedGasCostType {
			log.Fatal(
				"Precompile "+contract+"'s "+name+"GasCost's implementer has the wrong type",
				"\n\texpected:\t", expectedGasCostType, "\n\tbut have:\t", gascost.Type,
			)
		}

		methods[id] = PrecompileMethod{
			name,
			method,
			purity,
			handler,
			gascost,
			reflect.ValueOf(implementer),
		}
	}

	// provide the implementer mechanisms to emit logs for the solidity events

	supportedIndices := map[string]struct{}{
		// the solidity value types: https://docs.soliditylang.org/en/v0.8.9/types.html
		"address": {},
		"bytes32": {},
		"bool":    {},
	}
	for i := 8; i <= 256; i += 8 {
		supportedIndices["int"+strconv.Itoa(i)] = struct{}{}
		supportedIndices["uint"+strconv.Itoa(i)] = struct{}{}
	}

	for _, event := range source.Events {
		name := event.RawName

		var needs = []reflect.Type{
			reflect.TypeOf(&vm.EVM{}), // where the emit goes
		}
		for _, arg := range event.Inputs {
			needs = append(needs, arg.Type.GetType())

			if arg.Indexed {
				_, ok := supportedIndices[arg.Type.String()]
				if !ok {
					log.Fatal(
						"Please change the solidity for precompile ", contract,
						"'s event ", name, ":\n\tEvent indices of type ",
						arg.Type.String(), " are not supported",
					)
				}
			}
		}

		context := "Precompile " + contract + "'s implementer"
		ofType := " of type\n\tfunc "

		field, ok := implementerType.Elem().FieldByName(name)
		if !ok {
			log.Fatal(context, " is missing a field for event ", name, ofType, needs)
		}
		costField, ok := implementerType.Elem().FieldByName(name + "GasCost")
		if !ok {
			log.Fatal(context, " is missing a GasCost field for event ", name)
		}

		uint64Type := []reflect.Type{reflect.TypeOf(uint64(0))}
		expectedFieldType := reflect.FuncOf(needs, []reflect.Type{}, false)
		expectedCostType := reflect.FuncOf(needs[1:], uint64Type, false)

		if field.Type != expectedFieldType {
			log.Fatal(
				context, "'s field for event", name, "has the wrong type\n",
				"\texpected:\t", expectedFieldType, "\n\tbut have:\t", field.Type,
			)
		}
		if costField.Type != expectedCostType {
			log.Fatal(
				context, "'s field for event", name+"GasCost", "has the wrong type\n",
				"\texpected:\t", expectedCostType, "\n\tbut have:\t", costField.Type,
			)
		}

		structFields := reflect.ValueOf(implementer).Elem()
		fieldPointer := structFields.FieldByName(name)
		costPointer := structFields.FieldByName(name + "GasCost")

		// we can't capture `event` since the for loop will change its value
		capturedEvent := event

		emit := func(args []reflect.Value) []reflect.Value {

			//nolint:errcheck
			evm := args[0].Interface().(*vm.EVM)
			state := evm.StateDB
			args = args[1:]

			// Filter by index'd into data and topics. Indexed values, even if ultimately hashed,
			// aren't supposed to have their contents stored in the general-purpose data portion.
			var dataValues []interface{}
			var topicValues []interface{}
			dataInputs := make(abi.Arguments, 0, len(args))
			topicInputs := make(abi.Arguments, 0, 3)

			for i := 0; i < len(args); i++ {
				if !capturedEvent.Inputs[i].Indexed {
					dataValues = append(dataValues, args[i].Interface())
					dataInputs = append(dataInputs, capturedEvent.Inputs[i])
				} else {
					topicValues = append(topicValues, args[i].Interface())
					topicInputs = append(topicInputs, capturedEvent.Inputs[i])
				}
			}

			data, err := dataInputs.PackValues(dataValues)
			if err != nil {
				// in production we'll just revert, but for now this
				// will catch implementation errors
				log.Fatal(
					"Could not pack values for event ", name, "\nargs ", args,
					"\nvalues ", dataValues, "\ntopics", topicValues, "\nerror ", err,
				)
			}

			topics := []common.Hash{capturedEvent.ID}

			for i, input := range topicInputs {
				// Geth provides infrastructure for packing arrays of values,
				// so we create an array with just the value we want to pack.

				packable := []interface{}{topicValues[i]}
				bytes, err := abi.Arguments{input}.PackValues(packable)
				if err != nil {
					// in production we'll just revert, but for now this
					// will catch implementation errors
					log.Fatal(
						"Could not pack values for event ", name, "\nargs ", args,
						"\nvalues ", dataValues, "\ntopics", topicValues, "\nerror ", err,
					)
				}

				var topic [32]byte

				if len(bytes) > 32 {
					topic = *(*[32]byte)(crypto.Keccak256(bytes))
				} else {
					offset := 32 - len(bytes)
					copy(topic[offset:], bytes)
				}

				topics = append(topics, topic)
			}

			event := &types.Log{
				Address:     address,
				Topics:      topics,
				Data:        data,
				BlockNumber: evm.Context.BlockNumber.Uint64(),
				// Geth will set all other fields, which include
				//   TxHash, TxIndex, Index, and Removed
			}

			state.AddLog(event)
			return []reflect.Value{}
		}

		cost := func(args []reflect.Value) []reflect.Value {
			return []reflect.Value{}
		}

		fieldPointer.Set(reflect.MakeFunc(field.Type, emit))
		costPointer.Set(reflect.MakeFunc(costField.Type, cost))

		events[name] = PrecompileEvent{
			name,
			event,
		}
	}

	return address, Precompile{
		methods,
		events,
	}
}

func Precompiles() map[addr]ArbosPrecompile {

	//nolint:gocritic
	hex := func(s string) addr {
		return common.HexToAddress(s)
	}

	contracts := make(map[addr]ArbosPrecompile)

	insert := func(address addr, impl ArbosPrecompile) {
		contracts[address] = impl
	}

	insert(makePrecompile(templates.ArbSysMetaData, &ArbSys{Address: hex("64")}))
	insert(makePrecompile(templates.ArbInfoMetaData, &ArbInfo{Address: hex("65")}))
	insert(makePrecompile(templates.ArbAddressTableMetaData, &ArbAddressTable{Address: hex("66")}))
	insert(makePrecompile(templates.ArbBLSMetaData, &ArbBLS{Address: hex("67")}))
	insert(makePrecompile(templates.ArbFunctionTableMetaData, &ArbFunctionTable{Address: hex("68")}))
	insert(makePrecompile(templates.ArbosTestMetaData, &ArbosTest{Address: hex("69")}))
	insert(makePrecompile(templates.ArbOwnerMetaData, &ArbOwner{Address: hex("6b")}))
	insert(makePrecompile(templates.ArbGasInfoMetaData, &ArbGasInfo{Address: hex("6c")}))
	insert(makePrecompile(templates.ArbAggregatorMetaData, &ArbAggregator{Address: hex("6d")}))
	insert(makePrecompile(templates.ArbRetryableTxMetaData, &ArbRetryableTx{Address: hex("6e")}))
	insert(makePrecompile(templates.ArbStatisticsMetaData, &ArbStatistics{Address: hex("6f")}))
	insert(makePrecompile(templates.ArbDebugMetaData, &ArbDebug{Address: hex("ff")}))

	return contracts
}

// determine the amount of gas to charge for calling a precompile
func (p Precompile) GasToCharge(input []byte) uint64 {

	if len(input) < 4 {
		// ArbOS precompiles always have canonical method selectors
		return 0
	}
	id := *(*[4]byte)(input)
	method, ok := p.methods[id]
	if !ok {
		// method does not exist
		return 0
	}

	args, err := method.template.Inputs.Unpack(input[4:])
	if err != nil {
		// calldata does not match the method's signature
		return 0
	}

	reflectArgs := []reflect.Value{
		method.implementer,
	}
	for _, arg := range args {
		reflectArgs = append(reflectArgs, reflect.ValueOf(arg))
	}

	// we checked earlier that gascost() returns a uint64
	return method.gascost.Func.Call(reflectArgs)[0].Interface().(uint64)
}

// call a precompile in typed form, deserializing its inputs and serializing its outputs
func (p Precompile) Call(
	input []byte,
	precompileAddress common.Address,
	actingAsAddress common.Address,
	caller common.Address,
	value *big.Int,
	readOnly bool,
	evm *vm.EVM,
) (output []byte, err error) {

	if len(input) < 4 {
		// ArbOS precompiles always have canonical method selectors
		return nil, vm.ErrExecutionReverted
	}
	id := *(*[4]byte)(input)
	method, ok := p.methods[id]
	if !ok {
		// method does not exist
		return nil, vm.ErrExecutionReverted
	}

	if method.purity >= view && actingAsAddress != precompileAddress {
		// should not access precompile superpowers when not acting as the precompile
		return nil, vm.ErrExecutionReverted
	}

	if method.purity >= write && readOnly {
		// tried to write to global state in read-only mode
		return nil, vm.ErrExecutionReverted
	}

	if method.purity < payable && value.Sign() != 0 {
		// tried to pay something that's non-payable
		return nil, vm.ErrExecutionReverted
	}

	reflectArgs := []reflect.Value{
		method.implementer,
		reflect.ValueOf(caller),
	}

	switch method.purity {
	case pure:
	case view:
		reflectArgs = append(reflectArgs, reflect.ValueOf(evm))
	case write:
		reflectArgs = append(reflectArgs, reflect.ValueOf(evm))
	case payable:
		reflectArgs = append(reflectArgs, reflect.ValueOf(evm))
		reflectArgs = append(reflectArgs, reflect.ValueOf(value))
	default:
		log.Fatal("Unknown state mutability ", method.purity)
	}

	args, err := method.template.Inputs.Unpack(input[4:])
	if err != nil {
		// calldata does not match the method's signature
		return nil, vm.ErrExecutionReverted
	}
	for _, arg := range args {
		reflectArgs = append(reflectArgs, reflect.ValueOf(arg))
	}

	reflectResult := method.handler.Func.Call(reflectArgs)
	resultCount := len(reflectResult) - 1
	if !reflectResult[resultCount].IsNil() {
		// the last arg is always the error status
		return nil, vm.ErrExecutionReverted
	}
	result := make([]interface{}, resultCount)
	for i := 0; i < resultCount; i++ {
		result[i] = reflectResult[i].Interface()
	}

	encoded, err := method.template.Outputs.PackValues(result)
	if err != nil {
		// in production we'll just revert, but for now this
		// will catch implementation errors
		log.Fatal("Could not encode precompile result ", err)
	}
	return encoded, nil
}

func (p Precompile) Precompile() Precompile {
	return p
}
