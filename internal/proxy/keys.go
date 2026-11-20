package proxy

// KeyType is how the proxy reads the binary form of a tracked table's key
// column (see Gate.Tracked).
type KeyType uint8

const (
	KeyInt2 KeyType = iota + 1
	KeyInt4
	KeyInt8
	KeyUUID
	KeyText // text, varchar, bpchar, name: the bytes as they are
)

// MaxReadKeys is how many key values the proxy takes from one read.
const MaxReadKeys = 1000

// RowKey names one row of a tracked table.
type RowKey struct {
	Table uint32 // the table's OID
	Text  string // the key as the type's output function writes it
}

// ReadKeys is what the proxy took from the answer to a tracked read.
type ReadKeys struct {
	Keys   []RowKey // from the rows sent, in order, at most MaxReadKeys
	Tables []uint32 // the tracked tables the result had a column of, in column order, each once
	Param  *Param   // the value bound to Statement.KeyParam; nil when there is none
}
