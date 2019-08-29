// Package epochtime implements the Oasis timekeeping backend.
package epochtime

import (
	"context"
	"fmt"
	"strings"

	flag "github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/oasislabs/ekiden/go/epochtime/api"
	"github.com/oasislabs/ekiden/go/epochtime/tendermint"
	"github.com/oasislabs/ekiden/go/epochtime/tendermint_mock"
	"github.com/oasislabs/ekiden/go/tendermint/service"
)

const (
	cfgBackend            = "epochtime.backend"
	cfgTendermintInterval = "epochtime.tendermint.interval"
)

// Flags has the configuration flags.
var Flags = flag.NewFlagSet("", flag.ContinueOnError)

// New constructs a new Backend based on the configuration flags.
func New(ctx context.Context, tmService service.TendermintService) (api.Backend, error) {
	backend := viper.GetString(cfgBackend)
	switch strings.ToLower(backend) {
	case tendermint.BackendName:
		interval := viper.GetInt64(cfgTendermintInterval)
		return tendermint.New(ctx, tmService, interval)
	case tendermintmock.BackendName:
		return tendermintmock.New(ctx, tmService)
	default:
		return nil, fmt.Errorf("epochtime: unsupported backend: '%v'", backend)
	}
}

func init() {
	Flags.String(cfgBackend, tendermint.BackendName, "Epoch time backend")
	Flags.Int64(cfgTendermintInterval, 86400, "Epoch interval (in blocks)")

	_ = viper.BindPFlags(Flags)
}
