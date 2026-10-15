// Package redaction holds the request every redaction of the server is
// tested with (the stderr logs, the log stream, the audit, the access logs
// and the live trace): one table, one predicate (domain.IsSensitiveParam).
package redaction

import "strings"

// Query is a raw query string with a secret in every kind of sensitive
// parameter: SigV4 and SigV2 presigned URLs, session tokens, the signed
// URLs of the distributions, names holding token, password or secret, and
// the usual credentials of the applications a distribution serves.
const Query = "X-Amz-Security-Token=SECRETSESSIONTOKEN123&token=MYTOKEN&password=hunter2&Signature=SIGV2SECRET" +
	"&AWSAccessKeyId=AKV2SECRET&X-Amz-Credential=CREDSECRET%2F20261015&X-Amz-Signature=SIGV4SECRET&client_secret=CLIENTSECRET" +
	"&Policy=POLICYSECRET&Key-Pair-Id=KEYPAIRSECRET&api_key=APIKEYSECRET&apikey=APIKEY2SECRET&auth=AUTHQSECRET" +
	"&sig=SIGSHORTSECRET&jwt=JWTSECRET&sid=SIDSECRET&prefix=photos%2F&max-keys=10"

// Secrets returns the values of Query no redaction may keep.
func Secrets() []string {
	return []string{"SECRETSESSIONTOKEN123", "MYTOKEN", "hunter2", "SIGV2SECRET", "AKV2SECRET", "CREDSECRET", "SIGV4SECRET",
		"CLIENTSECRET", "POLICYSECRET", "KEYPAIRSECRET", "APIKEYSECRET", "APIKEY2SECRET", "AUTHQSECRET", "SIGSHORTSECRET",
		"JWTSECRET", "SIDSECRET"}
}

// Kept returns the pairs of Query every redaction keeps.
func Kept() []string { return []string{"prefix=photos%2F", "max-keys=10"} }

// Leaks returns the secrets of Query found in text.
func Leaks(text string) []string {
	var out []string
	for _, s := range Secrets() {
		if strings.Contains(text, s) {
			out = append(out, s)
		}
	}
	return out
}
