package plist

import (
	"encoding"
	"fmt"
	"reflect"
	"time"

	"github.com/hashicorp/go-multierror"
)

type incompatibleDecodeTypeError struct {
	dest reflect.Type
	src  string // type name (from cfValue)
}

func (u *incompatibleDecodeTypeError) Error() string {
	return fmt.Sprintf("plist: type mismatch: tried to decode plist type `%v' into value of type `%v'", u.src, u.dest)
}

var (
	plistUnmarshalerType = reflect.TypeOf((*Unmarshaler)(nil)).Elem()
	textUnmarshalerType  = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
	uidType              = reflect.TypeOf(UID(0))
	stringType           = reflect.TypeOf("")
)

func isEmptyInterface(v reflect.Value) bool {
	return v.Kind() == reflect.Interface && v.NumMethod() == 0
}

func (p *Decoder) unmarshalPlistInterface(pval cfValue, unmarshalable Unmarshaler) error {
	return unmarshalable.UnmarshalPlist(func(i interface{}) error {
		return p.unmarshal(pval, reflect.ValueOf(i))
	})
}

func (p *Decoder) unmarshalTextInterface(pval cfString, unmarshalable encoding.TextUnmarshaler) error {
	return unmarshalable.UnmarshalText([]byte(pval))
}

func (p *Decoder) unmarshalTime(pval cfDate, val reflect.Value) {
	val.Set(reflect.ValueOf(time.Time(pval)))
}

func (p *Decoder) unmarshalLaxString(s string, val reflect.Value) error {
	switch val.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i := mustParseInt(s, 10, 64)
		val.SetInt(i)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		i := mustParseUint(s, 10, 64)
		val.SetUint(i)
		return nil
	case reflect.Float32, reflect.Float64:
		f := mustParseFloat(s, 64)
		val.SetFloat(f)
		return nil
	case reflect.Bool:
		b := mustParseBool(s)
		val.SetBool(b)
		return nil
	case reflect.Struct:
		if val.Type() == timeType {
			t, err := time.Parse(textPlistTimeLayout, s)
			if err != nil {
				return err
			}
			val.Set(reflect.ValueOf(t.In(time.UTC)))
			return nil
		}
		fallthrough
	default:
		return &incompatibleDecodeTypeError{val.Type(), "string"}
	}
}

func (p *Decoder) unmarshal(pval cfValue, val reflect.Value) error {
	if pval == nil {
		return nil
	}

	for val.Kind() == reflect.Ptr {
		if val.IsNil() {
			val.Set(reflect.New(val.Type().Elem()))
		}
		val = val.Elem()
	}

	if isEmptyInterface(val) {
		v := p.valueInterface(pval)
		val.Set(reflect.ValueOf(v))
		return nil
	}

	incompatibleTypeError := &incompatibleDecodeTypeError{val.Type(), pval.typeName()}

	if receiver, can := implementsInterface(val, plistUnmarshalerType); can {
		return p.unmarshalPlistInterface(pval, receiver.(Unmarshaler))
	}

	// time.Time implements TextMarshaler, but we need to parse it as RFC3339
	if date, ok := pval.(cfDate); ok {
		if val.Type() == timeType {
			p.unmarshalTime(date, val)
			return nil
		}
		return incompatibleTypeError
	}

	if val.Type() != timeType {
		if receiver, can := implementsInterface(val, textUnmarshalerType); can {
			if str, ok := pval.(cfString); ok {
				return p.unmarshalTextInterface(str, receiver.(encoding.TextUnmarshaler))
			}
			return incompatibleTypeError
		}
	}

	typ := val.Type()

	switch pval := pval.(type) {
	case cfString:
		if val.Kind() == reflect.String {
			val.SetString(string(pval))
			return nil
		}
		if p.lax {
			return p.unmarshalLaxString(string(pval), val)
		}
		return incompatibleTypeError

	case *cfNumber:
		switch val.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			val.SetInt(int64(pval.value))
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			val.SetUint(pval.value)
		default:
			return incompatibleTypeError
		}
		return nil

	case *cfReal:
		if val.Kind() == reflect.Float32 || val.Kind() == reflect.Float64 {
			// TODO: Consider warning on a downcast (storing a 64-bit value in a 32-bit reflect)
			val.SetFloat(pval.value)
			return nil
		}
		return incompatibleTypeError

	case cfBoolean:
		if val.Kind() == reflect.Bool {
			val.SetBool(bool(pval))
			return nil
		}
		return incompatibleTypeError

	case cfData:
		if val.Kind() != reflect.Slice && val.Kind() != reflect.Array {
			return incompatibleTypeError
		}

		if typ.Elem().Kind() != reflect.Uint8 {
			return incompatibleTypeError
		}

		b := []byte(pval)
		switch val.Kind() {
		case reflect.Slice:
			val.SetBytes(b)
		case reflect.Array:
			if val.Len() < len(b) {
				return fmt.Errorf("plist: attempted to unmarshal %d bytes into a byte array of size %d", len(b), val.Len())
			}
			sval := reflect.ValueOf(b)
			reflect.Copy(val, sval)
		}
		return nil

	case cfUID:
		if val.Type() == uidType {
			val.SetUint(uint64(pval))
			return nil
		}
		switch val.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			val.SetInt(int64(pval))
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			val.SetUint(uint64(pval))
		default:
			return incompatibleTypeError
		}
		return nil

	case *cfArray:
		return p.unmarshalArray(pval, val)

	case *cfDictionary:
		return p.unmarshalDictionary(pval, val)

	default:
		return fmt.Errorf("plist: unknown type %T", pval)
	}
}

func (p *Decoder) unmarshalArray(a *cfArray, val reflect.Value) error {
	var resultErr error
	var n int
	if val.Kind() == reflect.Slice {
		// Slice of element values.
		// Grow slice.
		cnt := len(a.values) + val.Len()
		if cnt > val.Cap() {
			ncap := val.Cap()
			for ncap < cnt {
				ncap = growSliceCap(ncap)
			}
			new := reflect.MakeSlice(val.Type(), val.Len(), ncap)
			reflect.Copy(new, val)
			val.Set(new)
		}
		n = val.Len()
		val.SetLen(cnt)
	} else if val.Kind() == reflect.Array {
		if len(a.values) > val.Cap() {
			return fmt.Errorf("plist: attempted to unmarshal %d values into an array of size %d", len(a.values), val.Cap())
		}
	} else {
		return &incompatibleDecodeTypeError{val.Type(), a.typeName()}
	}

	// Recur to read element into slice.
	for _, sval := range a.values {
		if err := p.unmarshal(sval, val.Index(n)); err != nil {
			resultErr = multierror.Append(resultErr, fmt.Errorf("element %d: %w", n, err))
		}
		n++
	}

	return resultErr
}

func growSliceCap(cap int) int {
	if cap == 0 {
		return 4
	} else if cap < 1024 {
		return cap * 2 // Double for small slices
	} else {
		return cap + cap/4 // Increase by 25% for large slices
	}
}

func (p *Decoder) unmarshalDictionary(dict *cfDictionary, val reflect.Value) error {
	typ := val.Type()
	switch val.Kind() {
	case reflect.Struct:
		tinfo, err := getTypeInfo(typ)
		if err != nil {
			return err
		}

		entries := make(map[string]cfValue, len(dict.keys))
		for i, k := range dict.keys {
			sval := dict.values[i]
			entries[k] = sval
		}

		var resultErr error

		for _, finfo := range tinfo.fields {
			if ent, ok := entries[finfo.name]; ok {
				fieldVal := finfo.valueForWriting(val)
				if fieldVal.CanSet() {
					if err := p.unmarshal(ent, fieldVal); err != nil {
						resultErr = multierror.Append(resultErr, fmt.Errorf("field %q: %w", finfo.name, err))
					}
				} else {
					resultErr = multierror.Append(resultErr,
						fmt.Errorf("field %q not settable", finfo.name))
				}
			}
		}

		return resultErr

	case reflect.Map:
		if val.IsNil() {
			val.Set(reflect.MakeMap(typ))
		}

		if !stringType.ConvertibleTo(val.Type().Key()) {
			return fmt.Errorf("plist: attempt to decode dictionary into map with non-string key type `%v'", val.Type().Key())
		}

		var resultErr error

		for i, k := range dict.keys {
			sval := dict.values[i]

			keyv := reflect.ValueOf(k).Convert(typ.Key())
			mapElem := reflect.New(typ.Elem()).Elem()

			if err := p.unmarshal(sval, mapElem); err != nil {
				resultErr = multierror.Append(resultErr, fmt.Errorf("map key %q: %w", k, err))
				continue
			}

			val.SetMapIndex(keyv, mapElem)
		}

		return resultErr

	default:
		return &incompatibleDecodeTypeError{typ, dict.typeName()}
	}
}

/* *Interface is modelled after encoding/json */
func (p *Decoder) valueInterface(pval cfValue) interface{} {
	switch pval := pval.(type) {
	case cfString:
		return string(pval)
	case *cfNumber:
		if pval.signed {
			return int64(pval.value)
		}
		return pval.value
	case *cfReal:
		if pval.wide {
			return pval.value
		} else {
			return float32(pval.value)
		}
	case cfBoolean:
		return bool(pval)
	case *cfArray:
		return p.arrayInterface(pval)
	case *cfDictionary:
		return p.dictionaryInterface(pval)
	case cfData:
		return []byte(pval)
	case cfDate:
		return time.Time(pval)
	case cfUID:
		return UID(pval)
	}
	return nil
}

func (p *Decoder) arrayInterface(a *cfArray) []interface{} {
	out := make([]interface{}, len(a.values))
	for i, subv := range a.values {
		out[i] = p.valueInterface(subv)
	}
	return out
}

func (p *Decoder) dictionaryInterface(dict *cfDictionary) map[string]interface{} {
	out := make(map[string]interface{})
	for i, k := range dict.keys {
		subv := dict.values[i]
		out[k] = p.valueInterface(subv)
	}
	return out
}
