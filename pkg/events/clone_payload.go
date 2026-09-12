package events

import "reflect"

// A graph memo prevents cycles and preserves repeated references within each
// copied payload. Slice length is part of its view identity; capacity is not a
// wire property and is limited to length in the independently owned copy.
type payloadVisit struct {
	typ    reflect.Type
	ptr    uintptr
	length int
}

type payloadCopier struct {
	seen map[payloadVisit]reflect.Value
}

func clonePayloadGraph(value any) any {
	if value == nil {
		return nil
	}
	c := payloadCopier{seen: make(map[payloadVisit]reflect.Value)}
	return c.copy(reflect.ValueOf(value)).Interface()
}

func (c *payloadCopier) copy(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(c.copy(v.Elem()))
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		key := payloadVisit{typ: v.Type(), ptr: v.Pointer()}
		if found, ok := c.seen[key]; ok {
			return found
		}
		out := reflect.New(v.Type().Elem())
		if out.Type() != v.Type() {
			out = out.Convert(v.Type())
		}
		c.seen[key] = out
		out.Elem().Set(c.copy(v.Elem()))
		return out
	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		key := payloadVisit{typ: v.Type(), ptr: uintptr(v.UnsafePointer())}
		if found, ok := c.seen[key]; ok {
			return found
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		c.seen[key] = out
		it := v.MapRange()
		for it.Next() {
			out.SetMapIndex(c.copy(it.Key()), c.copy(it.Value()))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		key := payloadVisit{typ: v.Type(), ptr: v.Pointer(), length: v.Len()}
		if found, ok := c.seen[key]; ok {
			return found
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		c.seen[key] = out
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(c.copy(v.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(c.copy(v.Index(i)))
		}
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := 0; i < v.NumField(); i++ {
			if out.Field(i).CanSet() {
				out.Field(i).Set(c.copy(v.Field(i)))
			}
		}
		return out
	default:
		return v
	}
}
