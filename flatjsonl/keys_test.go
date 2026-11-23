package flatjsonl

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_toSnakeCase(t *testing.T) {
	assert.Equal(t, "", toSnakeCase(""))
	assert.Equal(t, "foo", toSnakeCase("Foo"))
	assert.Equal(t, "foo_bar", toSnakeCase("FooBar"))
	assert.Equal(t, "foo_bar", toSnakeCase("Foo_Bar"))
	assert.Equal(t, "foo_bar", toSnakeCase("_Foo_Bar"))
	assert.Equal(t, "foo_bar", toSnakeCase("__Foo-Bar"))
	assert.Equal(t, "foo_bar varchar(500)", toSnakeCase("__Foo-Bar VARCHAR(500)"))
	assert.Equal(t, "foo_bar_0 varchar(500)", toSnakeCase("__Foo-Bar[0] VARCHAR(500)"))
}

func TestTransposePrefix_match(t *testing.T) {
	key := func(path ...string) flKey {
		return flKey{path: path}
	}

	// Single-level array transpose.
	tp := parseTransposePrefixes(map[string]string{".tags": "tags"})[0]
	k := key("tags", "[2]")

	assert.True(t, tp.match(&k))
	assert.Equal(t, "tags", k.transposeDst)
	assert.Equal(t, intOrString{t: TypeInt, i: 2}, k.transposeKey)
	assert.Equal(t, "._value", k.transposeTrimmed)
	assert.False(t, k.transposeHasParent())
	assert.Equal(t, "2", k.transposeRowKey())

	// Key equal to prefix is not transposed.
	assert.False(t, tp.match(&flKey{path: []string{"tags"}}))

	// Prefix matches whole segments only, not string prefixes.
	assert.False(t, tp.match(&flKey{path: []string{"tags2", "[0]"}}))

	// Single-level object transpose.
	tp = parseTransposePrefixes(map[string]string{".tokens": "tokens"})[0]
	k = key("tokens", "foo", "a")

	assert.True(t, tp.match(&k))
	assert.Equal(t, intOrString{t: TypeString, s: "foo"}, k.transposeKey)
	assert.Equal(t, ".a", k.transposeTrimmed)
	assert.False(t, k.transposeHasParent())

	// Nested transpose captures parent array index.
	tp = parseTransposePrefixes(map[string]string{".shipments.[].cells": "cell"})[0]
	k = key("shipments", "[1]", "cells", "[3]", "code")

	assert.True(t, tp.match(&k))
	assert.Equal(t, "cell", k.transposeDst)
	assert.Equal(t, intOrString{t: TypeInt, i: 3}, k.transposeKey)
	assert.Equal(t, ".code", k.transposeTrimmed)
	assert.True(t, k.transposeHasParent())
	assert.Equal(t, intOrString{t: TypeInt, i: 1}, k.transposeParentKey())
	assert.Equal(t, "1\x1f3", k.transposeRowKey())

	// Wildcard does not match object properties.
	assert.False(t, tp.match(&flKey{path: []string{"shipments", "foo", "cells", "[0]", "code"}}))

	// Wildcard does not match missing array level.
	assert.False(t, tp.match(&flKey{path: []string{"shipments", "cells", "[0]", "code"}}))

	// Scalar array item under nested prefix.
	k = key("shipments", "[0]", "cells", "[2]")
	assert.True(t, tp.match(&k))
	assert.Equal(t, "._value", k.transposeTrimmed)

	// More specific prefix wins regardless of config map order.
	tps := parseTransposePrefixes(map[string]string{
		".shipments":          "shipment",
		".shipments.[].cells": "cell",
	})
	k = key("shipments", "[1]", "cells", "[0]", "w")

	assert.True(t, tps[0].match(&k))
	assert.Equal(t, "cell", k.transposeDst)

	k = key("shipments", "[1]", "id")
	assert.False(t, tps[0].match(&k))
	assert.True(t, tps[1].match(&k))
	assert.Equal(t, "shipment", k.transposeDst)
	assert.False(t, k.transposeHasParent())
}

func TestParseArrayIndex(t *testing.T) {
	i, ok := parseArrayIndex("[12]")
	assert.True(t, ok)
	assert.Equal(t, 12, i)

	for _, seg := range []string{"", "[]", "[-1]", "[a]", "[1", "1]", "abc", "[1]x"} {
		_, ok := parseArrayIndex(seg)
		assert.False(t, ok, seg)
	}
}
