// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package commands

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/mattermost/mattermost-server/v6/model"

	"github.com/mattermost/mmctl/v6/client"
	"github.com/mattermost/mmctl/v6/printer"
)

var (
	insecureSignatureAlgorithms = map[x509.SignatureAlgorithm]bool{
		x509.SHA1WithRSA:   true,
		x509.DSAWithSHA1:   true,
		x509.ECDSAWithSHA1: true,
	}
	expectedSocketMode = os.ModeSocket | 0600
)

func CheckVersionMatch(version, serverVersion string) (bool, error) {
	mmctlVersionParsed, err := semver.NewVersion(version)
	if err != nil {
		return false, errors.Wrapf(err, "Cannot parse version range %s", version)
	}

	serverVersionParsed, err := semver.NewVersion(serverVersion)
	if err != nil {
		return false, errors.Wrapf(err, "Cannot parse version range %s", serverVersion)
	}

	if serverVersionParsed.Major() != mmctlVersionParsed.Major() {
		return false, nil
	}

	if mmctlVersionParsed.Minor() > serverVersionParsed.Minor() {
		return false, nil
	}

	return true, nil
}

func withClient(fn func(c client.Client, cmd *cobra.Command, args []string) error) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if viper.GetBool("local") {
			c, err := InitUnixClient(viper.GetString("local-socket-path"))
			if err != nil {
				return err
			}
			printer.SetServerAddres("local instance")
			return fn(c, cmd, args)
		}

		c, serverVersion, err := InitClient(viper.GetBool("insecure-sha1-intermediate"), viper.GetBool("insecure-tls-version"))
		if err != nil {
			return err
		}

		if Version != "unspecified" { // unspecified version indicates that we are on dev mode.
			valid, err := CheckVersionMatch(Version, serverVersion)
			if err != nil {
				return fmt.Errorf("could not check version mismatch: %w", err)
			}
			if !valid {
				if viper.GetBool("strict") {
					printer.PrintError("ERROR: server version " + serverVersion + " doesn't match with mmctl version " + Version + ". Strict flag is set, so the command will not be run")
					os.Exit(1)
				}
				if !viper.GetBool("suppress-warnings") {
					printer.PrintWarning("server version " + serverVersion + " doesn't match mmctl version " + Version)
				}
			}
		}

		printer.SetServerAddres(c.APIURL)
		return fn(c, cmd, args)
	}
}

func localOnlyPrecheck(cmd *cobra.Command, args []string) {
	local := viper.GetBool("local")
	if !local {
		fmt.Fprintln(os.Stderr, "This command can only be run in local mode")
		os.Exit(1)
	}
}

func disableLocalPrecheck(cmd *cobra.Command, args []string) {
	local := viper.GetBool("local")
	if local {
		fmt.Fprintln(os.Stderr, "This command cannot be run in local mode")
		os.Exit(1)
	}
}

func isValidChain(chain []*x509.Certificate) bool {
	// check all certs but the root one
	certs := chain[:len(chain)-1]

	for _, cert := range certs {
		if _, ok := insecureSignatureAlgorithms[cert.SignatureAlgorithm]; ok {
			return false
		}
	}
	return true
}

func VerifyCertificates(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
	// loop over certificate chains
	for _, chain := range verifiedChains {
		if isValidChain(chain) {
			return nil
		}
	}
	return fmt.Errorf("insecure algorithm found in the certificate chain. Use --insecure-sha1-intermediate flag to ignore. Aborting")
}

func NewAPIv4Client(instanceURL string, allowInsecureSHA1, allowInsecureTLS bool) *model.Client4 {
	client := model.NewAPIv4Client(instanceURL)
	userAgent := fmt.Sprintf("mmctl/%s (%s)", Version, runtime.GOOS)
	client.HTTPHeader = map[string]string{"User-Agent": userAgent}

	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	if allowInsecureTLS {
		tlsConfig.MinVersion = tls.VersionTLS10
	}

	if !allowInsecureSHA1 {
		tlsConfig.VerifyPeerCertificate = VerifyCertificates
	}

	client.HTTPClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
		},
	}

	return client
}

func InitClientWithUsernameAndPassword(username, password, instanceURL string, allowInsecureSHA1, allowInsecureTLS bool) (*model.Client4, string, error) {
	client := NewAPIv4Client(instanceURL, allowInsecureSHA1, allowInsecureTLS)

	_, resp, err := client.Login(username, password)
	if err != nil {
		return nil, "", checkInsecureTLSError(err, allowInsecureTLS)
	}
	return client, resp.ServerVersion, nil
}

func InitClientWithMFA(username, password, mfaToken, instanceURL string, allowInsecureSHA1, allowInsecureTLS bool) (*model.Client4, string, error) {
	client := NewAPIv4Client(instanceURL, allowInsecureSHA1, allowInsecureTLS)
	_, resp, err := client.LoginWithMFA(username, password, mfaToken)
	if err != nil {
		return nil, "", checkInsecureTLSError(err, allowInsecureTLS)
	}
	return client, resp.ServerVersion, nil
}

func InitClientWithCredentials(credentials *Credentials, allowInsecureSHA1, allowInsecureTLS bool) (*model.Client4, string, error) {
	client := NewAPIv4Client(credentials.InstanceURL, allowInsecureSHA1, allowInsecureTLS)

	client.AuthType = model.HeaderBearer
	client.AuthToken = credentials.AuthToken

	_, resp, err := client.GetMe("")
	if err != nil {
		return nil, "", checkInsecureTLSError(err, allowInsecureTLS)
	}

	return client, resp.ServerVersion, nil
}

func InitClient(allowInsecureSHA1, allowInsecureTLS bool) (*model.Client4, string, error) {
	credentials, err := GetCurrentCredentials()
	if err != nil {
		return nil, "", err
	}
	return InitClientWithCredentials(credentials, allowInsecureSHA1, allowInsecureTLS)
}

func InitWebSocketClient() (*model.WebSocketClient, error) {
	credentials, err := GetCurrentCredentials()
	if err != nil {
		return nil, err
	}
	client, appErr := model.NewWebSocketClient4(strings.Replace(credentials.InstanceURL, "http", "ws", 1), credentials.AuthToken)
	if appErr != nil {
		return nil, errors.Wrap(appErr, "unable to create the websockets connection")
	}
	return client, nil
}

func InitUnixClient(socketPath string) (*model.Client4, error) {
	if err := checkValidSocket(socketPath); err != nil {
		return nil, err
	}

	return model.NewAPIv4SocketClient(socketPath), nil
}

func checkInsecureTLSError(err error, allowInsecureTLS bool) error {
	if (strings.Contains(err.Error(), "tls: protocol version not supported") ||
		strings.Contains(err.Error(), "tls: server selected unsupported protocol version")) && !allowInsecureTLS {
		return errors.New("won't perform action through an insecure TLS connection. Please add --insecure-tls-version to bypass this check")
	}
	return err
}
