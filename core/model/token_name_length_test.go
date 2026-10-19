//nolint:testpackage // White-box regression exercises private model or runtime invariants.
package model

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Token names are limited in characters, like the varchar(32) column, so a
// name written in Chinese may use all 32 characters.
func TestTokenNameLimitCountsCharacters(t *testing.T) {
	require.NoError(t, (&Token{Name: EmptyNullString(strings.Repeat("密", 32))}).BeforeSave(nil))
	require.NoError(t, (&Token{Name: EmptyNullString(strings.Repeat("a", 32))}).BeforeSave(nil))
	require.Error(t, (&Token{Name: EmptyNullString(strings.Repeat("密", 33))}).BeforeSave(nil))
	require.Error(t, (&Token{Name: EmptyNullString(strings.Repeat("a", 33))}).BeforeSave(nil))
}
