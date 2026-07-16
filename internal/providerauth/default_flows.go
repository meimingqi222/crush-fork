package providerauth

import (
	"context"
	"errors"
	"time"

	hyperprovider "github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/oauth/copilot"
	"github.com/charmbracelet/crush/internal/oauth/hyper"
)

func DefaultFactories() map[FlowKey]FlowFactory {
	return map[FlowKey]FlowFactory{
		{ProviderID: hyperprovider.Name, Method: AuthMethodDeviceCode}: FlowFactoryFunc(func(string) Flow { return hyperFlow{} }),
		{ProviderID: "copilot", Method: AuthMethodDeviceCode}:          FlowFactoryFunc(func(string) Flow { return copilotFlow{} }),
	}
}

type hyperFlow struct{}

func (hyperFlow) Run(ctx context.Context, emit func(Prompt) error) (Credential, error) {
	challenge, err := hyper.InitiateDeviceAuth(ctx)
	if err != nil {
		return Credential{}, err
	}
	if err := emit(Prompt{
		Kind: AuthMethodDeviceCode, VerificationURI: challenge.VerificationURL,
		UserCode: challenge.UserCode, ExpiresAt: time.Now().Add(time.Duration(challenge.ExpiresIn) * time.Second),
	}); err != nil {
		return Credential{}, err
	}
	refreshToken, err := hyper.PollForToken(ctx, challenge.DeviceCode, challenge.ExpiresIn)
	if err != nil {
		return Credential{}, err
	}
	token, err := hyper.ExchangeToken(ctx, refreshToken)
	if err != nil {
		return Credential{}, err
	}
	introspection, err := hyper.IntrospectToken(ctx, token.AccessToken)
	if err != nil {
		return Credential{}, err
	}
	if !introspection.Active {
		return Credential{}, errors.New("provider token is inactive")
	}
	return Credential{Token: token}, nil
}

type copilotFlow struct{}

func (copilotFlow) Run(ctx context.Context, emit func(Prompt) error) (Credential, error) {
	challenge, err := copilot.RequestDeviceCode(ctx)
	if err != nil {
		return Credential{}, err
	}
	if err := emit(Prompt{
		Kind: AuthMethodDeviceCode, VerificationURI: challenge.VerificationURI,
		UserCode: challenge.UserCode, ExpiresAt: time.Now().Add(time.Duration(challenge.ExpiresIn) * time.Second),
	}); err != nil {
		return Credential{}, err
	}
	token, err := copilot.PollForToken(ctx, challenge)
	if err != nil {
		return Credential{}, err
	}
	return Credential{Token: token}, nil
}
