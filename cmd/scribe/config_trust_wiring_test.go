package main

import (
	"reflect"
	"testing"
)

// The trust layer is wired by convention in four places: the
// sensitiveConfig struct, sensitiveFrom, applyTo, and sensitiveDiff.
// Forgetting any one when adding a field creates a silent trust hole —
// the key looks locked but a pushed change slips through unwarned.
// This test makes that mistake impossible: every leaf of
// sensitiveConfig must survive the applyTo→sensitiveFrom round-trip,
// alter the trust hash, and surface a `scribe config diff` row.

// fillTrustValue sets v to a deterministic non-zero value of its type.
func fillTrustValue(v reflect.Value) {
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(true)
	case reflect.String:
		v.SetString("trust-probe")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(7)
	case reflect.Slice:
		elem := reflect.New(v.Type().Elem()).Elem()
		fillTrustValue(elem)
		v.Set(reflect.Append(reflect.MakeSlice(v.Type(), 0, 1), elem))
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key()).Elem()
		fillTrustValue(key)
		val := reflect.New(v.Type().Elem()).Elem()
		fillTrustValue(val)
		m.SetMapIndex(key, val)
		v.Set(m)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fillTrustValue(p.Elem())
		v.Set(p)
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Field(i).CanSet() {
				fillTrustValue(v.Field(i))
			}
		}
	default:
		panic("fillTrustValue: unhandled kind " + v.Kind().String())
	}
}

// sensitiveProbes returns one sensitiveConfig per mutated leaf: each
// top-level field gets a probe, and struct fields additionally get one
// probe per sub-field — that finer grain is what catches a new
// SourcesConfig sub-key missing its own sensitiveDiff row.
func sensitiveProbes() map[string]sensitiveConfig {
	probes := map[string]sensitiveConfig{}
	typ := reflect.TypeOf(sensitiveConfig{})
	for i := range typ.NumField() {
		field := typ.Field(i)
		if field.Type.Kind() == reflect.Struct {
			for j := range field.Type.NumField() {
				var s sensitiveConfig
				sub := reflect.ValueOf(&s).Elem().Field(i).Field(j)
				if !sub.CanSet() {
					continue
				}
				fillTrustValue(sub)
				probes[field.Name+"."+field.Type.Field(j).Name] = s
			}
			continue
		}
		var s sensitiveConfig
		fillTrustValue(reflect.ValueOf(&s).Elem().Field(i))
		probes[field.Name] = s
	}
	return probes
}

func TestSensitiveConfigWiringComplete(t *testing.T) {
	var zero sensitiveConfig
	probes := sensitiveProbes()
	if len(probes) < 9 {
		t.Fatalf("only %d probes — reflection enumeration broke?", len(probes))
	}
	for name, probe := range probes {
		// A field that doesn't survive the round-trip is locked on
		// paper but not in practice: enforceConfigTrust would revert it
		// to a zero value (or never snapshot it at all).
		cfg := &ScribeConfig{}
		probe.applyTo(cfg)
		if got := sensitiveFrom(cfg); !reflect.DeepEqual(got, probe) {
			t.Errorf("%s: lost in applyTo→sensitiveFrom round-trip\n got %+v\nwant %+v", name, got, probe)
		}
		// Drift detection: the hash gates enforcement...
		if probe.hash() == zero.hash() {
			t.Errorf("%s: change does not alter the trust hash", name)
		}
		// ...and the diff is what the user reviews before re-trusting.
		// A key that drifts without a diff row warns but shows nothing.
		if len(sensitiveDiff(zero, probe)) == 0 {
			t.Errorf("%s: change produces no `scribe config diff` row", name)
		}
	}
}
