module github.com/apernet/hysteria/core/v2

go 1.25.0

toolchain go1.25.1

require (
	github.com/apernet/quic-go v0.59.1
	github.com/stretchr/testify v1.11.1
	go.uber.org/goleak v1.2.1
	golang.org/x/exp v0.0.0-20240506185415-9bf2ced13842
	golang.org/x/time v0.12.0
)

replace github.com/apernet/quic-go => github.com/onesyue/quic-go v0.59.1-yue.7

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	github.com/rogpeppe/go-internal v1.12.0 // indirect
	github.com/stretchr/objx v0.5.2 // indirect
	golang.org/x/crypto v0.53.0 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.39.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
