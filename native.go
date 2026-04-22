package mirageslack

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	jsonnet "github.com/google/go-jsonnet"
	"github.com/google/go-jsonnet/ast"
)

// registerNativeFunctions attaches ssm() and env() to the jsonnet VM.
func registerNativeFunctions(ctx context.Context, vm *jsonnet.VM) {
	vm.NativeFunction(&jsonnet.NativeFunction{
		Name:   "env",
		Params: []ast.Identifier{"name"},
		Func: func(args []any) (any, error) {
			name, ok := args[0].(string)
			if !ok {
				return nil, fmt.Errorf("env: expected string argument")
			}
			return os.Getenv(name), nil
		},
	})

	resolver := newSSMResolver(ctx)
	vm.NativeFunction(&jsonnet.NativeFunction{
		Name:   "ssm",
		Params: []ast.Identifier{"path"},
		Func: func(args []any) (any, error) {
			path, ok := args[0].(string)
			if !ok {
				return nil, fmt.Errorf("ssm: expected string argument")
			}
			return resolver.Lookup(path)
		},
	})
}

// ssmResolver wraps an AWS SSM client to resolve parameter store values lazily.
// The AWS SDK config and client are initialized on first use so that the
// jsonnet VM can be constructed even when SSM is not used.
type ssmResolver struct {
	ctx    context.Context
	client *ssm.Client
	err    error
	done   bool
}

func newSSMResolver(ctx context.Context) *ssmResolver {
	return &ssmResolver{ctx: ctx}
}

func (r *ssmResolver) Lookup(path string) (string, error) {
	if !r.done {
		cfg, err := awsconfig.LoadDefaultConfig(r.ctx)
		if err != nil {
			r.err = fmt.Errorf("load aws config: %w", err)
		} else {
			r.client = ssm.NewFromConfig(cfg)
		}
		r.done = true
	}
	if r.err != nil {
		return "", r.err
	}

	out, err := r.client.GetParameter(r.ctx, &ssm.GetParameterInput{
		Name:           aws.String(path),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return "", fmt.Errorf("ssm get parameter %q: %w", path, err)
	}
	if out.Parameter == nil || out.Parameter.Value == nil {
		return "", fmt.Errorf("ssm parameter %q has no value", path)
	}
	return *out.Parameter.Value, nil
}
