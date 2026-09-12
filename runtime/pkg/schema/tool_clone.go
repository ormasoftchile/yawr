package schema

import "reflect"

// CloneToolDef returns an independent copy, including frozen executable closures
// and native default values. Tool schema values are acyclic data, not resources.
func CloneToolDef(definition *ToolDef) *ToolDef {
	return cloneToolValue(reflect.ValueOf(definition)).Interface().(*ToolDef)
}

// CloneToolAction copies a retained action without changing numeric value types.
func CloneToolAction(action *ToolAction) *ToolAction {
	return cloneToolValue(reflect.ValueOf(action)).Interface().(*ToolAction)
}

func cloneToolValue(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		copy := reflect.New(value.Type()).Elem()
		if value.Kind() == reflect.Pointer {
			copy.Set(reflect.New(value.Type().Elem()))
			copy.Elem().Set(cloneToolValue(value.Elem()))
		} else {
			copy.Set(cloneToolValue(value.Elem()))
		}
		return copy
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		copy := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			copy.SetMapIndex(iter.Key(), cloneToolValue(iter.Value()))
		}
		return copy
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		copy := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			copy.Index(i).Set(cloneToolValue(value.Index(i)))
		}
		return copy
	case reflect.Struct:
		copy := reflect.New(value.Type()).Elem()
		copy.Set(value)
		for i := 0; i < value.NumField(); i++ {
			if value.Type().Field(i).IsExported() {
				copy.Field(i).Set(cloneToolValue(value.Field(i)))
			}
		}
		return copy
	default:
		return value
	}
}
