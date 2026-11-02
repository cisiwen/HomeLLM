package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	cognitosrp "github.com/alexrudd/cognito-srp/v4"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"
)

// cognitoIDToken signs in with the user pool's USER_SRP_AUTH flow (the only one the app client
// enables) and returns an ID token. The password never leaves this machine.
func cognitoIDToken(ctx context.Context, region, poolID, clientID, username, password string) (string, error) {
	srp, err := cognitosrp.NewCognitoSRP(username, password, poolID, clientID, nil)
	if err != nil {
		return "", err
	}
	svc := cognitoidentityprovider.New(cognitoidentityprovider.Options{Region: region, Credentials: aws.AnonymousCredentials{}})
	init, err := svc.InitiateAuth(ctx, &cognitoidentityprovider.InitiateAuthInput{
		AuthFlow:       types.AuthFlowTypeUserSrpAuth,
		ClientId:       aws.String(srp.GetClientId()),
		AuthParameters: srp.GetAuthParams(),
	})
	if err != nil {
		return "", fmt.Errorf("cognito sign-in: %w", err)
	}
	if init.ChallengeName != types.ChallengeNameTypePasswordVerifier {
		return "", fmt.Errorf("cognito sign-in: unsupported challenge %q", init.ChallengeName)
	}
	responses, err := srp.PasswordVerifierChallenge(init.ChallengeParameters, time.Now())
	if err != nil {
		return "", err
	}
	res, err := svc.RespondToAuthChallenge(ctx, &cognitoidentityprovider.RespondToAuthChallengeInput{
		ChallengeName:      types.ChallengeNameTypePasswordVerifier,
		ChallengeResponses: responses,
		ClientId:           aws.String(srp.GetClientId()),
	})
	if err != nil {
		return "", fmt.Errorf("cognito sign-in: %w", err)
	}
	if res.AuthenticationResult == nil || res.AuthenticationResult.IdToken == nil {
		return "", errors.New("cognito sign-in: no ID token returned (is MFA or a password change required?)")
	}
	return *res.AuthenticationResult.IdToken, nil
}
