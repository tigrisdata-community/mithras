package bash

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/go-git/go-billy/v5"
	"github.com/openai/openai-go/v3"
	bashexec "github.com/tigrisdata-community/mithras/internal/codeinterpreter/bash"
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
	var i Input
	if err := json.Unmarshal(data, &i); err != nil {
		return nil, fmt.Errorf("can't parse json: %w", err)
	}

	result, err := bashexec.Run(ctx, fsys, i.Command)
	if err != nil {
		return nil, fmt.Errorf("can't execute bash: %w", err)
	}

	resultBytes, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("can't marshal result bytes: %w", err)
	}

	return resultBytes, nil
}
