package bash

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/go-git/go-billy/v5"
	"github.com/openai/openai-go/v3"
)

var (
	ErrNoCommand = errors.New("bash: no command provided")
)

type Input struct {
	Command string `json:"command" jsonschema:"Bash command to run"`
}

func (i Input) Valid() error {
	if i.Command == "" {
		return ErrNoCommand
	}

	return nil
}

type Impl struct{}

func (Impl) Name() string {
	return "bash"
}

func (Impl) Usage() openai.FunctionDefinitionParam {
	return openai.FunctionDefinitionParam{
		Name:        "bash",
		Description: openai.String("Execute bash commands in a safe sandbox."),
		Parameters: openai.FunctionParameters{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]string{
					"type": "string",
				},
			},
			"required": []string{"command"},
		},
	}
}

func (Impl) Valid(data []byte) error {
	var i Input
	if err := json.Unmarshal(data, &i); err != nil {
		return fmt.Errorf("can't parse json: %w", err)
	}

	return i.Valid()
}

func (Impl) Run(ctx context.Context, fsys billy.Filesystem, data []byte) ([]byte, error) {
	_ = ctx
	_ = fsys
	_ = data
	return nil, errors.New("bash: not implemented")
}
