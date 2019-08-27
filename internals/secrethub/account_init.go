package secrethub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/secrethub/secrethub-go/pkg/secrethub/credentials"

	"github.com/secrethub/secrethub-cli/internals/cli/clip"
	"github.com/secrethub/secrethub-cli/internals/cli/progress"
	"github.com/secrethub/secrethub-cli/internals/cli/ui"

	"github.com/secrethub/secrethub-go/internals/api"
	"github.com/secrethub/secrethub-go/pkg/secrethub"
)

// Constants that define the waiting periods for polling the server to check whether the credential is added.
const (
	WaitTimeout = 10 * time.Minute
	WaitDelta   = 2 * time.Second
)

// Errors
var (
	ErrCredentialNotGenerated      = errMain.Code("credential_not_generated").Error("Generate a device credential first. Use the command without --continue to do so.")
	ErrCredentialAlreadyAuthorized = errMain.Code("credential_already_authorized").ErrorPref("Already started initializing for %s. Use the command with --continue to continue.")
	ErrAddCredentialTimeout        = errMain.Code("add_credential_timeout").Error("Timed out. Run again with --continue to continue initializing your account.")
	ErrAccountAlreadyInitialized   = errMain.Code("account_already_initialized").Error("This account has already been initialized. We currently do not support using an account on multiple devices.")
	ErrAccountAlreadyConnected     = errMain.Code("account_already_connected").ErrorPref("Already connected as initialized account %s.")
)

// AccountInitCommand creates, stores and outputs a credential.
type AccountInitCommand struct {
	io              ui.IO
	useClipboard    bool
	noWait          bool
	isContinue      bool
	force           bool
	credentialStore CredentialConfig
	clipper         clip.Clipper
	progressPrinter progress.Printer
	newClient       newClientFunc
}

// NewAccountInitCommand creates a new AccountInitCommand.
func NewAccountInitCommand(io ui.IO, newClient newClientFunc, credentialStore CredentialConfig) *AccountInitCommand {
	return &AccountInitCommand{
		io:              io,
		credentialStore: credentialStore,
		clipper:         clip.NewClipboard(),
		progressPrinter: progress.NewPrinter(io.Stdout(), 500*time.Millisecond),
		newClient:       newClient,
	}
}

// Register registers the command, arguments and flags on the provided Registerer.
func (cmd *AccountInitCommand) Register(r Registerer) {
	clause := r.Command("init", "Connect a first device to your SecretHub account.")
	clause.Flag("clip", "Copy the credential's public component to the clipboard instead of printing it to stdout.").Short('c').BoolVar(&cmd.useClipboard)
	clause.Flag("no-wait", "Do not hang waiting for the credential's public component to be added to the account and instead exit after outputting the credential's public component. To finish initializing the account, use the --continue flag after adding the credential to the account.").BoolVar(&cmd.noWait)
	clause.Flag("continue", "Continue initializing the account. Use this when a credential has already been generated by a previous execution of the command.").BoolVar(&cmd.isContinue)
	registerForceFlag(clause).BoolVar(&cmd.force)

	BindAction(clause, cmd.Run)
}

// Run creates a credential for this CLI and an account key for the credential.
func (cmd *AccountInitCommand) Run() error {

	if !cmd.isContinue {
		credentialPath := cmd.credentialStore.ConfigDir().Credential().Path

		if cmd.credentialStore.ConfigDir().Credential().Exists() {
			client, err := cmd.newClient()
			if err != nil {
				return err
			}

			authenticated, err := cmd.isAuthenticated(client)
			if err != nil {
				return err
			}

			if authenticated {
				me, err := client.Users().Me()
				if err != nil {
					return err
				}

				if me.PublicKey == nil {
					if !cmd.force {
						confirmed, err := ui.AskYesNo(
							cmd.io,
							fmt.Sprintf("Already started initializing for %s. Do you want to continue?", me.PrettyName()),
							ui.DefaultNo,
						)
						if err == ui.ErrCannotAsk {
							return ErrCredentialAlreadyAuthorized(me.PrettyName())
						} else if err != nil {
							return err
						}

						if !confirmed {
							fmt.Fprintln(cmd.io.Stdout(), "Aborting.")
							return nil
						}
					}
					return cmd.createAccountKey()
				}

				keyedForClient, err := client.Accounts().Keys().Exists()
				if err != nil {
					return err
				}

				if keyedForClient {
					return ErrAccountAlreadyConnected(me.PrettyName())
				}

				return ErrAccountAlreadyInitialized

			}

			if !cmd.force {
				confirmed, err := ui.AskYesNo(
					cmd.io,
					fmt.Sprintf("Found account credentials at %s, do you wish to overwrite them?", credentialPath),
					ui.DefaultNo,
				)
				if err == ui.ErrCannotAsk {
					return ErrLocalAccountFound
				} else if err != nil {
					return err
				}

				if !confirmed {
					fmt.Fprintln(cmd.io.Stdout(), "Aborting.")
					return nil
				}
			}
		}

		fmt.Fprintf(
			cmd.io.Stdout(),
			"An account credential will be generated and stored at %s. "+
				"Losing this credential means you lose the ability to decrypt your secrets. "+
				"So keep it safe.\n",
			credentialPath,
		)

		credential := credentials.CreateKey()

		// Only prompt for a passphrase when the user hasn't used --force.
		// Otherwise, we assume the passphrase was intentionally not
		// configured to output a plaintext credential.
		var passphrase string
		if !cmd.credentialStore.IsPassphraseSet() && !cmd.force {
			var err error
			passphrase, err = ui.AskPassphrase(cmd.io, "Please enter a passphrase to protect your local credential (leave empty for no passphrase): ", "Enter the same passphrase again: ", 3)
			if err != nil {
				return err
			}
		}

		fmt.Fprint(cmd.io.Stdout(), "Generating credential...")
		err := credential.Create()
		if err != nil {
			return err
		}

		fmt.Fprintln(cmd.io.Stdout(), " Done")

		exportKey := credential.Key
		if passphrase != "" {
			exportKey = exportKey.Passphrase(credentials.FromString(passphrase))
		}

		exportedCredential, err := exportKey.Export()
		if err != nil {
			return err
		}
		err = cmd.credentialStore.ConfigDir().Credential().Write(exportedCredential)
		if err != nil {
			return err
		}

		verifierBytes, err := exportKey.Verifier().Verifier()
		if err != nil {
			return err
		}

		fingerprint, err := api.GetFingerprint(exportKey.Verifier().Type(), verifierBytes)
		if err != nil {
			return err
		}

		outBytes, err := json.Marshal(
			struct {
				Type        api.CredentialType `json:"type"`
				Fingerprint string             `json:"fingerprint"`
				Verifier    []byte             `json:"verifier"`
			}{
				Type:        exportKey.Verifier().Type(),
				Fingerprint: fingerprint,
				Verifier:    verifierBytes,
			},
		)
		if err != nil {
			return err
		}

		out := make([]byte, base64.RawURLEncoding.EncodedLen(len(outBytes)))
		base64.RawURLEncoding.Encode(out, outBytes)

		if cmd.useClipboard {
			err = cmd.clipper.WriteAll(out)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.io.Stdout(), "The credential's public component has been copied to the clipboard. To add the credential to your account, paste the clipboard contents in https://dashboard.secrethub.io/account-init")
		} else {
			fmt.Fprintln(cmd.io.Stdout(), "To add the credential to your account, paste the public component shown below in https://dashboard.secrethub.io/account-init")

			fmt.Fprintf(cmd.io.Stdout(), "\n%s\n", out)
		}
	} else {
		if !cmd.credentialStore.ConfigDir().Credential().Exists() {
			return ErrCredentialNotGenerated
		}
	}

	return cmd.createAccountKey()
}

// createAccountKey polls the server on /me/user until the credential is
// added to the account. When the credential is added to the account, it
// creates an account key for the credential.
func (cmd *AccountInitCommand) createAccountKey() error {
	client, err := cmd.newClient()
	if err != nil {
		return err
	}

	isAuthenticated, err := cmd.isAuthenticated(client)
	if err != nil {
		return err
	}

	if !isAuthenticated {
		if cmd.noWait {
			fmt.Fprintln(cmd.io.Stdout(), "Not waiting for credential to be added. To continue initializing your account after you have added the credential, run again with --continue.")
			return nil
		}
		fmt.Fprint(cmd.io.Stdout(), "Waiting for credential to be added...")

		authenticatedC, errC := cmd.waitForCredentialToBeAdded(client)

		select {
		case <-authenticatedC:
			fmt.Fprintln(cmd.io.Stdout(), " Done")
		case err := <-errC:
			fmt.Fprintln(cmd.io.Stdout(), " Failed")
			return err
		case <-time.After(WaitTimeout):
			fmt.Fprintln(cmd.io.Stdout(), " Failed")
			return ErrAddCredentialTimeout
		}
	}

	me, err := client.Users().Me()
	if err != nil {
		return err
	}

	if me.PublicKey != nil {
		keyedForClient, err := client.Accounts().Keys().Exists()
		if err != nil {
			return err
		}

		if keyedForClient {
			return ErrAccountAlreadyConnected(me.PrettyName())
		}

		return ErrAccountAlreadyInitialized
	}

	fmt.Fprint(cmd.io.Stdout(), "Finishing setup of your account...")

	key, err := cmd.credentialStore.Import()
	if err != nil {
		return err
	}

	verifierBytes, err := key.Verifier().Verifier()
	if err != nil {
		return err
	}

	fingerprint, err := api.GetFingerprint(key.Verifier().Type(), verifierBytes)
	if err != nil {
		return err
	}

	_, err = client.Accounts().Keys().Create(fingerprint, key.Encrypter())
	if err != nil {
		fmt.Fprintln(cmd.io.Stdout(), " Failed")
		return err
	}

	fmt.Fprintln(cmd.io.Stdout(), " Done")

	return nil
}

// waitForCredentialToBeAdded returns a channel on which is returned when the credential is added and a channel
// on which an error is returned if one occurs.
func (cmd *AccountInitCommand) waitForCredentialToBeAdded(client secrethub.ClientAdapter) (chan bool, chan error) {
	errc := make(chan error, 1)
	c := make(chan bool, 1)
	go func() {
		for {
			isAuthenticated, err := cmd.isAuthenticated(client)
			if err != nil {
				errc <- err
				break
			}
			if isAuthenticated {
				c <- true
				break
			}
			time.Sleep(WaitDelta)
		}
	}()
	return c, errc
}

func (cmd *AccountInitCommand) isAuthenticated(client secrethub.ClientAdapter) (bool, error) {
	_, err := client.Users().Me()
	if err == api.ErrSignatureNotVerified {
		return false, nil
	} else if err == nil {
		return true, nil
	}
	return false, err
}
