/*
MIT License

Copyright (c) 2017 Dmitri Shuralyov

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

// Package graphqljson provides a function for decoding JSON
// into a GraphQL query data structure.
package graphqljson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

var (
	objectBeginToken   = json.Delim('{')
	objectEndToken     = json.Delim('}')
	arrayBeginToken    = json.Delim('[')
	arrayEndToken      = json.Delim(']')
	mapStringInterface = reflect.TypeOf(map[string]interface{}{})
)

// Reference: https://blog.gopheracademy.com/advent-2017/custom-json-unmarshaler-for-graphql-client/

// UnmarshalData parses the JSON-encoded GraphQL response data and stores
// the result in the GraphQL query data structure pointed to by v.
//
// The implementation is created on top of the JSON tokenizer available
// in "encoding/json".Decoder.
func UnmarshalData(data json.RawMessage, v interface{}) error {
	d := newDecoder(bytes.NewBuffer(data))
	if err := d.Decode(v); err != nil {
		return fmt.Errorf(": %w", err)
	}

	// TODO: この処理が本当に必要かは今後検討
	tok, err := d.jsonDecoder.Token()
	switch err {
	case io.EOF:
		// Expect to get io.EOF. There shouldn't be any more
		// tokens left after we've decoded v successfully.
		return nil
	case nil:
		return fmt.Errorf("invalid token '%v' after top-level value", tok)
	}

	return fmt.Errorf("invalid token '%v' after top-level value", tok)
}

// Decoder is a JSON Decoder that performs custom unmarshaling behavior
// for GraphQL query data structures. It's implemented on top of a JSON tokenizer.
type Decoder struct {
	jsonDecoder *json.Decoder

	// Stack of what part of input JSON we're in the middle of - objects, arrays.
	parseState []json.Delim

	// Stacks of values where to unmarshal.
	// The top of each stack is the reflect.Value where to unmarshal next JSON value.
	//
	// The reason there's more than one stack is because we might be unmarshaling
	// a single JSON value into multiple GraphQL fragments or embedded structs, so
	// we keep track of them all.
	vs [][]reflect.Value
}

func newDecoder(r io.Reader) *Decoder {
	jsonDecoder := json.NewDecoder(r)
	jsonDecoder.UseNumber()

	return &Decoder{
		jsonDecoder: jsonDecoder,
	}
}

func followPtr(v reflect.Value) reflect.Value {
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	return v
}

func (d *Decoder) insideObject(tok json.Token) bool {
	return d.state() == objectBeginToken && tok != objectEndToken
}

func (d *Decoder) insideArray(tok json.Token) bool {
	return d.state() == arrayBeginToken && tok != arrayEndToken
}

// Decode decodes a single JSON value from d.tokenizer into v.
func (d *Decoder) Decode(v interface{}) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Ptr {
		return fmt.Errorf("cannot decode into non-pointer %T", v)
	}

	d.vs = [][]reflect.Value{{rv.Elem()}}
	if err := d.decode(); err != nil {
		return fmt.Errorf(": %w", err)
	}

	return nil
}

// decode decodes a single JSON value from d.tokenizer into d.vs.
func (d *Decoder) decode() error {
	// The loop invariant is that the top of each d.vs stack
	// is where we try to unmarshal the next JSON value we see.
	// var customField reflect.Value
	// var unmarshalJSON reflect.Value
loop:
	for len(d.vs) > 0 {
		tok, err := d.jsonDecoder.Token()

		if err == io.EOF {
			return errors.New("unexpected end of JSON input")
		} else if err != nil {
			return fmt.Errorf(": %w", err)
		}

		switch {
		// Are we inside an object and seeing next key (rather than end of object)?
		case d.insideObject(tok):
			key, ok := tok.(string)
			if !ok {
				return errors.New("unexpected non-key in JSON input")
			}
			someFieldExist := false
			continueLoop := false
			for i, dv := range d.vs {
				v := followPtr(dv[len(dv)-1])
				var f reflect.Value
				switch v.Kind() {
				case reflect.Struct:
					f = fieldByGraphQLName(v, key)
					if f.IsValid() {
						someFieldExist = true

						switch f.Kind() {
						case reflect.Map:
							f.Set(reflect.MakeMap(mapStringInterface))
							if err := d.jsonDecoder.Decode(f.Addr().Interface()); err != nil {
								return fmt.Errorf(": %w", err)
							}
							continueLoop = true
						default:
							d.vs[i] = append(dv, f)
						}
					}
				}
			}

			if !someFieldExist {
				return fmt.Errorf("struct field for %q doesn't exist in any of %v places to unmarshal", key, len(d.vs))
			}

			if continueLoop {
				continue loop
			}

			// We've just consumed the current token, which was the key.
			// Read the next token, which should be the value, and let the rest of code process it.
			tok, err = d.jsonDecoder.Token()

			if err == io.EOF {
				return errors.New("unexpected end of JSON input")
			} else if err != nil {
				return fmt.Errorf(": %w", err)
			}
		// Are we inside an array and seeing next value (rather than end of array)?
		case d.insideArray(tok):
			someSliceExist := false
			for i, dv := range d.vs {
				v := followPtr(dv[len(dv)-1])
				var f reflect.Value
				if v.Kind() == reflect.Slice {
					v.Set(reflect.Append(v, reflect.Zero(v.Type().Elem()))) // v = append(v, T).
					f = v.Index(v.Len() - 1)
					someSliceExist = true
				}
				d.vs[i] = append(dv, f)
			}
			if !someSliceExist {
				return fmt.Errorf("slice doesn't exist in any of %v places to unmarshal", len(d.vs))
			}
		}

		switch tok := tok.(type) {
		case string, json.Number, bool, nil:
			// Value.
			for _, dv := range d.vs {
				v := dv[len(dv)-1]
				if !v.IsValid() {
					continue
				}
				if err := unmarshalValue(tok, v); err != nil {
					return fmt.Errorf(": %w", err)
				}
			}
			d.popAllVs()

		case json.Delim:
			switch tok {
			case objectBeginToken:
				// Start of object.
				d.pushState(tok)
				frontier := make([]reflect.Value, len(d.vs)) // Places to look for GraphQL fragments/embedded structs.
				for i, dv := range d.vs {
					v := dv[len(dv)-1]
					frontier[i] = v
					// TODO: Do this recursively or not? Add a test case if needed.
					if v.Kind() == reflect.Ptr && v.IsNil() {
						v.Set(reflect.New(v.Type().Elem())) // v = new(T).
					}
				}
				// Find GraphQL fragments/embedded structs recursively, adding to frontier
				// as new ones are discovered and exploring them further.
				for len(frontier) > 0 {
					v := followPtr(frontier[0])
					frontier = frontier[1:]
					if v.Kind() != reflect.Struct {
						continue
					}
					for i := 0; i < v.NumField(); i++ {
						tf := v.Type().Field(i)
						if isGraphQLFragment(tf) || tf.Anonymous {
							f := v.Field(i)
							// Add GraphQL fragment or embedded struct.
							d.vs = append(d.vs, []reflect.Value{f})
							frontier = append(frontier, f)
						}
					}
				}
			case arrayBeginToken:
				// Start of array.

				d.pushState(tok)

				for _, dv := range d.vs {
					v := followPtr(dv[len(dv)-1])
					// TODO: Confirm this is needed, write a test case.
					// if v.Kind() == reflect.Ptr && v.IsNil() {
					//	v.Set(reflect.New(v.Type().Elem())) // v = new(T).
					//}

					// Reset slice to empty (in case it had non-zero initial value).
					if v.Kind() != reflect.Slice {
						continue
					}
					v.Set(reflect.MakeSlice(v.Type(), 0, 0)) // v = make(T, 0, 0).
				}
			case objectEndToken, arrayEndToken:
				// End of object or array.
				d.popAllVs()
				d.popState()
			default:
				return errors.New("unexpected delimiter in JSON input")
			}
		default:
			return errors.New("unexpected token in JSON input")
		}
	}

	return nil
}

// pushState pushes a new parse state s onto the stack.
func (d *Decoder) pushState(s json.Delim) {
	d.parseState = append(d.parseState, s)
}

// popState pops a parse state (already obtained) off the stack.
// The stack must be non-empty.
func (d *Decoder) popState() {
	d.parseState = d.parseState[:len(d.parseState)-1]
}

// state reports the parse state on top of stack, or 0 if empty.
func (d *Decoder) state() json.Delim {
	if l := len(d.parseState); l == 0 {
		return 0
	} else {
		return d.parseState[l-1]
	}
}

// popAllVs pops from all d.vs stacks, keeping only non-empty ones.
func (d *Decoder) popAllVs() {
	var nonEmpty [][]reflect.Value
	for _, dv := range d.vs {
		dv = dv[:len(dv)-1]
		if len(dv) > 0 {
			nonEmpty = append(nonEmpty, dv)
		}
	}
	d.vs = nonEmpty
}

// fieldByGraphQLName returns an exported struct field of struct v
// that matches GraphQL name, or invalid reflect.Value if none found.
func fieldByGraphQLName(v reflect.Value, name string) reflect.Value {
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		if f.PkgPath != "" {
			// Skip unexported field.
			continue
		}
		if hasGraphQLName(f, name) {
			return v.Field(i)
		}
	}

	return reflect.Value{}
}

// hasGraphQLName reports whether struct field f has GraphQL name.
func hasGraphQLName(f reflect.StructField, name string) bool {
	value, ok := f.Tag.Lookup("graphql")
	if !ok {
		// TODO: caseconv package is relatively slow. Optimize it, then consider using it here.
		// return caseconv.MixedCapsToLowerCamelCase(f.Name) == name
		return strings.EqualFold(f.Name, name)
	}
	value = strings.TrimSpace(value) // TODO: Parse better.
	if strings.HasPrefix(value, "...") {
		// GraphQL fragment. It doesn't have a name.
		return false
	}
	if i := strings.Index(value, "("); i != -1 {
		value = value[:i]
	}
	if i := strings.Index(value, ":"); i != -1 {
		value = value[:i]
	}

	return strings.TrimSpace(value) == name
}

// isGraphQLFragment reports whether struct field f is a GraphQL fragment.
func isGraphQLFragment(f reflect.StructField) bool {
	value, ok := f.Tag.Lookup("graphql")
	if !ok {
		return false
	}
	value = strings.TrimSpace(value) // TODO: Parse better.

	return strings.HasPrefix(value, "...")
}

// unmarshalValue unmarshals JSON value into v.
// v must be addressable and not obtained by the use of unexported
// struct fields, otherwise unmarshalValue will panic.
func unmarshalValue(value json.Token, v reflect.Value) error {
	b, err := json.Marshal(value) // TODO: Short-circuit (if profiling says it's worth it).
	if err != nil {
		return fmt.Errorf(": %w", err)
	}

	err = json.Unmarshal(b, v.Addr().Interface())
	if err != nil {
		return fmt.Errorf(": %w", err)
	}

	return nil
}
