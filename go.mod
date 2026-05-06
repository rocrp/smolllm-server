module github.com/rocry/smolllm-server

go 1.25.3

replace github.com/rocry/smolllm-go => ../smolllm-go

require (
	github.com/joho/godotenv v1.5.1
	github.com/openai/openai-go/v3 v3.34.0
	github.com/rocry/smolllm-go v0.0.0-00010101000000-000000000000
	github.com/stretchr/testify v1.11.1
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
)
