package kvstore

import (
	"context"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

type kvGetInput struct {
	Key string `json:"key" jsonschema:"description=The key to retrieve from the store"`
}

type kvSetInput struct {
	Key   string `json:"key" jsonschema:"description=The key to store"`
	Value string `json:"value" jsonschema:"description=The value associated with the key"`
}

type kvDeleteInput struct {
	Key string `json:"key" jsonschema:"description=The key to delete"`
}

type kvListInput struct{}

func Tools() ([]tool.BaseTool, error) {
	kvGetTool, err := utils.InferTool(
		"kv_get",
		"Get value by key from the key-value store. This tool can be executed concurrently with other read tools (e.g. kv_list).",
		func(_ context.Context, input *kvGetInput) (string, error) {
			return kvGet(input.Key)
		},
	)

	if err != nil {
		return nil, err
	}

	kvSetTool, err := utils.InferTool(
		"kv_set",
		"Set a key-value pair in the store. Must be executed individually and strictly without concurrency. Note: The store has space limits, so make sure to delete unused keys before calling kv_set.",
		func(_ context.Context, input *kvSetInput) (string, error) {
			err := kvSet(input.Key, input.Value)
			if err != nil {
				return "", err
			}

			return "success", nil
		},
	)

	if err != nil {
		return nil, err
	}

	kvDeleteTool, err := utils.InferTool(
		"kv_delete",
		"Delete a key from the key-value store. Must be executed individually and strictly without concurrency.",
		func(_ context.Context, input *kvDeleteInput) (string, error) {
			err := kvDelete(input.Key)
			if err != nil {
				return "", err
			}

			return "success", nil
		},
	)

	if err != nil {
		return nil, err
	}

	kvListTool, err := utils.InferTool(
		"kv_list",
		"List all keys currently available in the key-value store. This tool can be executed concurrently with other read tools (e.g. kv_get).",
		func(_ context.Context, input *kvListInput) ([]string, error) {
			return kvList(), nil
		},
	)

	if err != nil {
		return nil, err
	}

	return []tool.BaseTool{kvGetTool, kvSetTool, kvDeleteTool, kvListTool}, nil
}
