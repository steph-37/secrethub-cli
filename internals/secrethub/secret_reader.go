package secrethub

import "github.com/secrethub/secrethub-cli/internals/secrethub/tpl"

type secretReader struct {
	newClient newClientFunc
}

// newSecretReader wraps a client to implement tpl.SecretReader.
func newSecretReader(newClient newClientFunc) *secretReader {
	return &secretReader{
		newClient: newClient,
	}
}

// ReadSecret reads the secret using the provided client.
func (sr secretReader) ReadSecret(path string) (string, error) {
	client, err := sr.newClient()
	if err != nil {
		return "", err
	}

	secret, err := client.Secrets().Versions().GetWithData(path)
	if err != nil {
		return "", err
	}

	return string(secret.Data), nil
}

type bufferedSecretReader struct {
	secretReader tpl.SecretReader
	secretsRead  []string
}

// newBufferedSecretReader wraps a secret reader and stores the retrieved
// secret values for retrieval with the SecretsRead function.
func newBufferedSecretReader(sr tpl.SecretReader) *bufferedSecretReader {
	return &bufferedSecretReader{
		secretReader: sr,
		secretsRead:  []string{},
	}
}

// ReadSecret uses the underlying secret reader to read the secret
// and stores the result for retrieval with the SecretsRead function.
func (sr *bufferedSecretReader) ReadSecret(path string) (string, error) {
	secret, err := sr.secretReader.ReadSecret(path)

	if err == nil {
		sr.secretsRead = append(sr.secretsRead, secret)
	}

	return secret, err
}

// SecretsRead returns a list of values read with this secret reader.
func (sr bufferedSecretReader) SecretsRead() []string {
	return sr.secretsRead
}
