package utilities

// OpCode is an opcode of compiled path patterns.
type OpCode int

// These constants are the valid values of OpCode.
const (
	// OpNop does nothing
	OpNop = OpCode(iota)
	// OpPush pushes a component to stack
	OpPush
	// OpLitPush pushes a component to stack if it matches to the literal
	OpLitPush
	// OpPushM concatenates the remaining components and pushes it to stack
	OpPushM
	// OpConcatN pops N items from stack, concatenates them and pushes it back to stack
	OpConcatN
	// OpCapture pops an item and binds it to the variable
	OpCapture
	// OpEnd is the least positive invalid opcode.
	OpEnd
)
