package templates

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dagger/dagger/cmd/codegen/generator"
	"github.com/dagger/dagger/cmd/codegen/introspection"
)

func TestNullableObjectFieldFunction(t *testing.T) {
	funcs := goTemplateFuncs{
		CommonFunctions: generator.NewCommonFunctions("v1.0.0-beta.10", &FormatTypeFunc{}),
	}
	field := introspection.Field{
		Name:         "latestVersion",
		TypeRef:      &introspection.TypeRef{Kind: introspection.TypeKindObject, Name: "GitRef"},
		ParentObject: &introspection.Type{Name: "GitRepository"},
	}

	signature, err := funcs.fieldFunction(field, false, true)
	require.NoError(t, err)
	require.Equal(t, "func (r *GitRepository) LatestVersion(ctx context.Context) (*GitRef, error)", signature)
}
