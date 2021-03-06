package zavro

import (
	"encoding/binary"
	"errors"

	"github.com/brimsec/zq/zcode"
	"github.com/brimsec/zq/zng"
)

//XXX zq flattens records (i.e., id.orig_h, etc).  kafka target might want
// the records unflattened a la the zeek writer

// These errors shouldn't happen because the input should be type checked.
var ErrBadValue = errors.New("bad zng value in kavro translator")

func Encode(dst []byte, id uint32, r *zng.Record) ([]byte, error) {
	// build kafka/avro header
	var hdr [5]byte
	hdr[0] = 0
	binary.BigEndian.PutUint32(hdr[1:], uint32(id))
	dst = append(dst, hdr[:]...)
	// write value body seralized as avro
	return encodeRecord(dst, r.Type, r.Raw)
}

//XXX move this to zval
func zlen(zv zcode.Bytes) (int, error) {
	it := zcode.Iter(zv)
	cnt := 0
	for !it.Done() {
		_, _, err := it.Next()
		if err != nil {
			return 0, err
		}
		cnt++
	}
	return cnt, nil
}

func encodeVector(dst []byte, typ *zng.TypeArray, body zcode.Bytes) ([]byte, error) {
	if body == nil {
		return dst, nil
	}
	cnt, err := zlen(body)
	if err != nil {
		return nil, err
	}
	dst = appendVarint(dst, int64(cnt))
	inner := zng.InnerType(typ)
	it := zcode.Iter(body)
	for !it.Done() {
		body, container, err := it.Next()
		if err != nil {
			return nil, err
		}
		switch v := inner.(type) {
		case *zng.TypeRecord:
			if !container {
				return nil, ErrBadValue
			}
			dst, err = encodeRecord(dst, v, body)
			if err != nil {
				return nil, err
			}
		case *zng.TypeArray:
			if !container {
				return nil, ErrBadValue
			}
			dst, err = encodeVector(dst, v, body)
			if err != nil {
				return nil, err
			}
		case *zng.TypeSet:
			if !container {
				return nil, ErrBadValue
			}
			dst, err = encodeSet(dst, v, body)
			if err != nil {
				return nil, err
			}
		default:
			if container {
				return nil, ErrBadValue
			}
			dst, err = encodeScalar(dst, v, body)
			if err != nil {
				return nil, err
			}
		}
	}
	if cnt != 0 {
		// append 0-length block to indicate end of array
		dst = appendVarint(dst, int64(0))
	}
	return dst, nil
}

func encodeSet(dst []byte, typ *zng.TypeSet, body zcode.Bytes) ([]byte, error) {
	if body == nil {
		return dst, nil
	}
	inner := zng.InnerType(typ)
	if zng.IsContainerType(inner) {
		return nil, ErrBadValue
	}
	cnt, err := zlen(body)
	if err != nil {
		return nil, err
	}
	dst = appendVarint(dst, int64(cnt))
	it := zcode.Iter(body)
	for !it.Done() {
		body, container, err := it.Next()
		if err != nil {
			return nil, err
		}
		if container {
			return nil, ErrBadValue
		}
		dst, err = encodeScalar(dst, inner, body)
		if err != nil {
			return nil, err
		}
	}
	if cnt != 0 {
		// append 0-length block to indicate end of array
		dst = appendVarint(dst, int64(0))
	}
	return dst, nil
}

func encodeRecord(dst []byte, typ *zng.TypeRecord, body zcode.Bytes) ([]byte, error) {
	if body == nil {
		return dst, nil
	}
	it := zcode.Iter(body)
	for _, col := range typ.Columns {
		if it.Done() {
			return nil, ErrBadValue
		}
		body, container, err := it.Next()
		if err != nil {
			return nil, err
		}
		if body == nil {
			// unset field.  encode as the null type.
			dst = appendVarint(dst, 0)
			continue
		}
		// field is present.  encode the field union by referecing
		// the type's position in the union.
		dst = appendVarint(dst, 1)
		switch v := col.Type.(type) {
		case *zng.TypeRecord:
			if !container {
				return nil, ErrBadValue
			}
			dst, err = encodeRecord(dst, v, body)
			if err != nil {
				return nil, err
			}
		case *zng.TypeArray:
			if !container {
				return nil, ErrBadValue
			}
			dst, err = encodeVector(dst, v, body)
			if err != nil {
				return nil, err
			}
		case *zng.TypeSet:
			if !container {
				return nil, ErrBadValue
			}
			dst, err = encodeSet(dst, v, body)
			if err != nil {
				return nil, err
			}
		default:
			if container {
				return nil, ErrBadValue
			}
			dst, err = encodeScalar(dst, col.Type, body)
			if err != nil {
				return nil, err
			}
		}
	}
	return dst, nil
}

func appendVarint(dst []byte, v int64) []byte {
	var encoding [binary.MaxVarintLen64]byte
	n := binary.PutVarint(encoding[:], v)
	return append(dst, encoding[:n]...)
}

func appendCountedValue(dst, val []byte) []byte {
	dst = appendVarint(dst, int64(len(val)))
	return append(dst, val...)
}

func encodeScalar(dst []byte, typ zng.Type, body zcode.Bytes) ([]byte, error) {
	if body == nil {
		//XXX need to encode empty stuff
		return dst, nil
	}
	switch typ.ID() {
	case zng.IdIP:
		// IP addresses are turned into strings...
		ip, err := zng.DecodeIP(body)
		if err != nil {
			return nil, err
		}
		b := []byte(ip.String())
		return appendCountedValue(dst, b), nil

	case zng.IdBool:
		// bool is single byte 0 or 1
		v, err := zng.DecodeBool(body)
		if err != nil {
			return nil, err
		}
		if v {
			return append(dst, byte(1)), nil
		}
		return append(dst, byte(0)), nil

	case zng.IdInt64:
		v, err := zng.DecodeInt(body)
		if err != nil {
			return nil, err
		}
		return appendVarint(dst, v), nil

	case zng.IdUint64:
		// count is encoded as a uint64.  XXX return error on overdflow?
		v, err := zng.DecodeUint(body)
		if err != nil {
			return nil, err
		}
		return appendVarint(dst, int64(v)), nil

	case zng.IdFloat64:
		// avro says this is Java's doubleToLongBits...
		// we need to check if Go math lib is the same
		if len(body) != 8 {
			return nil, errors.New("double value not 8 bytes")
		}
		return append(dst, body...), nil

	case zng.IdDuration:
		// XXX map an interval to a microsecond time
		ns, err := zng.DecodeDuration(body)
		if err != nil {
			return nil, err
		}
		us := ns / 1000
		return appendVarint(dst, us), nil

	case zng.IdPort:
		// XXX map a port to an int
		port, err := zng.DecodePort(body)
		if err != nil {
			return nil, err
		}
		return appendVarint(dst, int64(port)), nil

	case zng.IdString, zng.IdBstring:
		s := typ.StringOf(body, zng.OutFormatZeek, false)
		return appendCountedValue(dst, []byte(s)), nil

	case zng.IdNet:
		// IP subnets are turned into strings...
		net, err := zng.DecodeNet(body)
		if err != nil {
			return nil, err
		}
		b := []byte(net.String())
		return appendCountedValue(dst, b), nil

	case zng.IdTime:
		// XXX map a nano to a microsecond time
		ts, err := zng.DecodeInt(body)
		if err != nil {
			return nil, err
		}
		us := ts / 1000
		return appendVarint(dst, us), nil

		/*
			case *zng.TypeRecord, *zng.TypeArray, *zng.TypeSet:
				panic("internal bug") //XXX
		*/
	default:
		panic("unknown type") //XXX
	}
}
