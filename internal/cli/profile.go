// Package cli implements the profile-first Capsule command boundary.
package cli

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.yaml.in/yaml/v3"
)

var profileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// The current CLL MySQL schema inherits database collation. Restrict v1 log
// names to lowercase ASCII without whitespace so distinct CLI targets cannot
// alias through case folding, accents or MySQL's trailing-space comparison.
var logName = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]{0,190}$`)

// Secret permits precisely one explicit source. Environment variables are only
// consulted when named here; they never override another profile setting.
type Secret struct {
	Value string `yaml:"value,omitempty" mapstructure:"value"`
	File  string `yaml:"file,omitempty" mapstructure:"file"`
	Env   string `yaml:"env,omitempty" mapstructure:"env"`
}

func (s Secret) validate() error {
	n := 0
	for _, v := range []string{s.Value, s.File, s.Env} {
		if v != "" {
			n++
		}
	}
	if n > 1 {
		return inputError("conflicting secret sources")
	}
	return nil
}
func (s Secret) resolve() (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	if s.File != "" {
		b, e := readProtected(s.File)
		if e != nil {
			return "", inputError("cannot read protected secret file")
		}
		s.Value = strings.TrimSpace(string(b))
	}
	if s.Env != "" {
		s.Value = os.Getenv(s.Env)
	}
	if (s.File != "" || s.Env != "") && s.Value == "" {
		return "", inputError("declared secret reference is empty")
	}
	return s.Value, nil
}
func (s Secret) redact() Secret {
	if s.Value != "" {
		s.Value = "[redacted]"
	}
	return s
}

type Profile struct {
	Name       string `yaml:"name" mapstructure:"name"`
	Type       string `yaml:"type" mapstructure:"type"`
	LogID      string `yaml:"log_id" mapstructure:"log_id"`
	Namespace  string `yaml:"namespace" mapstructure:"namespace"`
	StoreID    string `yaml:"store_id,omitempty" mapstructure:"store_id"`
	ReadOnly   bool   `yaml:"read_only,omitempty" mapstructure:"read_only"`
	Connection struct {
		Host     string `yaml:"host" mapstructure:"host"`
		Port     int    `yaml:"port" mapstructure:"port"`
		Database string `yaml:"database" mapstructure:"database"`
		TLS      string `yaml:"tls" mapstructure:"tls"`
	} `yaml:"connection" mapstructure:"connection"`
	Credentials struct {
		Username string `yaml:"username" mapstructure:"username"`
		Password Secret `yaml:"password,omitempty" mapstructure:"password"`
	} `yaml:"credentials" mapstructure:"credentials"`
	Signing     Secret   `yaml:"signing,omitempty" mapstructure:"signing"`
	TrustedKeys []string `yaml:"trusted_keys,omitempty" mapstructure:"trusted_keys"`
	Checkpoint  struct {
		Signing     Secret   `yaml:"signing,omitempty" mapstructure:"signing"`
		TrustedKeys []string `yaml:"trusted_keys,omitempty" mapstructure:"trusted_keys"`
		Endpoint    string   `yaml:"endpoint,omitempty" mapstructure:"endpoint"`
		PublicKey   string   `yaml:"public_key,omitempty" mapstructure:"public_key"`
		Token       Secret   `yaml:"token,omitempty" mapstructure:"token"`
	} `yaml:"checkpoint,omitempty" mapstructure:"checkpoint"`
}

func (p Profile) validate() error {
	if !profileName.MatchString(p.Name) || p.Type != "mysql" {
		return inputError("profile needs a valid name and mysql type")
	}
	if (p.LogID != "" && !logName.MatchString(p.LogID)) || (p.Namespace != "" && !profileName.MatchString(p.Namespace)) {
		return inputError("invalid log_id or namespace")
	}
	if p.LogID == "" && p.Namespace == "" {
		return inputError("configure an artifact namespace, a log_id, or both")
	}
	if p.Connection.Host == "" || p.Connection.Database == "" || p.Connection.Port < 1 || p.Connection.Port > 65535 {
		return inputError("profile needs a MySQL host, port and database")
	}
	if p.Connection.TLS != "true" && p.Connection.TLS != "false" {
		return inputError("mysql TLS must be true or false; insecure fallback is unsupported")
	}
	for _, s := range []Secret{p.Credentials.Password, p.Signing, p.Checkpoint.Signing, p.Checkpoint.Token} {
		if e := s.validate(); e != nil {
			return e
		}
	}
	if _, e := parseKeys(p.TrustedKeys); e != nil {
		return e
	}
	if _, e := parseKeys(p.Checkpoint.TrustedKeys); e != nil {
		return e
	}
	return nil
}
func parseKeys(values []string) ([]ed25519.PublicKey, error) {
	keys := make([]ed25519.PublicKey, 0, len(values))
	for _, v := range values {
		b, e := hex.DecodeString(v)
		if e != nil || len(b) != 32 {
			return nil, inputError("trusted keys must be 32-byte Ed25519 public keys in hex")
		}
		keys = append(keys, ed25519.PublicKey(b))
	}
	return keys, nil
}
func privateKey(s Secret) (ed25519.PrivateKey, error) {
	v, e := s.resolve()
	if e != nil {
		return nil, e
	}
	b, e := hex.DecodeString(v)
	if e != nil || len(b) != ed25519.SeedSize {
		return nil, inputError("signing secret must contain a 32-byte Ed25519 seed in hex")
	}
	return ed25519.NewKeyFromSeed(b), nil
}
func readProtected(path string) ([]byte, error) {
	info, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, inputError("file must be regular and accessible only to its owner")
	}
	return os.ReadFile(path)
}
func profilePath(name string) (string, error) {
	if !profileName.MatchString(name) {
		return "", inputError("explicit valid --profile is required")
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, e := os.UserHomeDir()
		if e != nil {
			return "", e
		}
		base = filepath.Join(home, ".config")
	}
	if !filepath.IsAbs(base) {
		return "", inputError("XDG_CONFIG_HOME must be absolute")
	}
	return filepath.Join(base, "capsule", "profiles", name+".yaml"), nil
}
func loadProfile(name string) (Profile, error) {
	path, e := profilePath(name)
	if e != nil {
		return Profile{}, e
	}
	raw, e := readProtected(path)
	if e != nil {
		return Profile{}, inputError("profile missing or not owner-protected")
	}
	var p Profile
	d := yaml.NewDecoder(bytes.NewReader(raw))
	d.KnownFields(true)
	if e = d.Decode(&p); e != nil {
		return p, inputError("invalid profile fields")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return p, inputError("profile must contain one document")
	}
	if p.Name != name {
		return p, inputError("profile filename and name differ")
	}
	return p, p.validate()
}

// atomicFile uses link for create-only admission and rename for explicit updates.
// fsync on both the file and directory makes profile changes restart-durable.
func atomicFile(path string, data []byte, replace bool) (err error) {
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".capsule-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() {
		if e := os.Remove(name); e != nil && !errors.Is(e, os.ErrNotExist) {
			err = errors.Join(err, e)
		}
	}()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if replace {
		info, e := os.Lstat(path)
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return inputError("update requires an existing owner-protected regular file")
		}
		err = os.Rename(name, path)
	} else {
		err = os.Link(name, path)
	}
	if err != nil {
		return err
	}
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	return errors.Join(d.Sync(), d.Close())
}
func saveProfile(p Profile, replace bool) error {
	if e := p.validate(); e != nil {
		return e
	}
	b, e := yaml.Marshal(p)
	if e != nil {
		return e
	}
	path, e := profilePath(p.Name)
	if e != nil {
		return e
	}
	return atomicFile(path, b, replace)
}
func profileCommands() *cobra.Command {
	group := &cobra.Command{Use: "profile", Short: "Create, inspect or explicitly update a named target"}
	show := &cobra.Command{Use: "show", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		p, e := selected(c)
		if e != nil {
			return e
		}
		p.Credentials.Password = p.Credentials.Password.redact()
		p.Signing = p.Signing.redact()
		p.Checkpoint.Signing = p.Checkpoint.Signing.redact()
		p.Checkpoint.Token = p.Checkpoint.Token.redact()
		return output(c, p)
	}}
	group.AddCommand(show)
	for _, update := range []bool{false, true} {
		verb := "create"
		if update {
			verb = "update"
		}
		c := &cobra.Command{Use: verb, Args: noArgs}
		f := c.Flags()
		f.String("name", "", "New profile name")
		f.Bool("interactive", false, "Ask for missing nonsecret connection fields")
		fields := map[string]string{"type": "type", "log-id": "log_id", "namespace": "namespace", "mysql-host": "connection.host", "mysql-database": "connection.database", "mysql-tls": "connection.tls", "mysql-user": "credentials.username", "checkpoint-endpoint": "checkpoint.endpoint", "checkpoint-public-key": "checkpoint.public_key"}
		for flag := range fields {
			f.String(flag, "", "Profile setting")
		}
		f.Int("mysql-port", 3306, "MySQL port")
		f.Bool("read-only", false, "Reject operations requiring writes")
		f.StringSlice("trusted-key", nil, "Trusted producer public key hex (repeatable)")
		f.StringSlice("checkpoint-trusted-key", nil, "Trusted checkpoint signer public key hex (repeatable)")
		secrets := map[string]string{"mysql-password": "credentials.password", "signing-key": "signing", "checkpoint-signing-key": "checkpoint.signing", "checkpoint-token": "checkpoint.token"}
		for flag := range secrets {
			f.String(flag, "", "Literal secret; prefer file/env reference")
			f.String(flag+"-file", "", "Owner-protected secret file")
			f.String(flag+"-env", "", "Explicit secret environment-variable name")
		}
		c.RunE = func(c *cobra.Command, _ []string) error {
			v := viper.New()
			v.SetConfigType("yaml")
			var prior Profile
			if update {
				p, e := selected(c)
				if e != nil {
					return e
				}
				prior = p
				b, e := yaml.Marshal(p)
				if e != nil {
					return e
				}
				if e = v.ReadConfig(bytes.NewReader(b)); e != nil {
					return e
				}
			} else {
				name, _ := c.Flags().GetString("name")
				v.SetDefault("name", name)
			}
			v.SetDefault("type", "mysql")
			v.SetDefault("namespace", "capsule")
			v.SetDefault("connection.tls", "true")
			v.SetDefault("connection.port", 3306)
			binds := map[string]string{}
			for flag, key := range fields {
				binds[flag] = key
			}
			for flag, key := range map[string]string{"mysql-port": "connection.port", "read-only": "read_only", "trusted-key": "trusted_keys", "checkpoint-trusted-key": "checkpoint.trusted_keys"} {
				binds[flag] = key
			}
			for flag, key := range secrets {
				binds[flag] = key + ".value"
				binds[flag+"-file"] = key + ".file"
				binds[flag+"-env"] = key + ".env"
			}
			for flag, key := range binds {
				if e := v.BindPFlag(key, c.Flags().Lookup(flag)); e != nil {
					return e
				}
			}
			var p Profile
			if e := v.UnmarshalExact(&p); e != nil {
				return inputError("invalid profile settings")
			}
			interactive, _ := c.Flags().GetBool("interactive")
			if interactive {
				reader := bufio.NewReader(c.InOrStdin())
				for _, item := range []struct {
					label  string
					target *string
				}{{"Name", &p.Name}, {"MySQL host", &p.Connection.Host}, {"Database", &p.Connection.Database}, {"Log ID (optional for artifact-only)", &p.LogID}, {"MySQL user", &p.Credentials.Username}} {
					if *item.target == "" {
						if _, e := fmt.Fprint(c.ErrOrStderr(), item.label+": "); e != nil {
							return e
						}
						s, e := reader.ReadString('\n')
						if e != nil {
							return inputError("guided configuration requires input; use flags for automation")
						}
						*item.target = strings.TrimSpace(s)
					}
				}
			}
			if update && prior.StoreID != "" && (p.LogID != prior.LogID || p.Namespace != prior.Namespace) {
				return inputError("initialized profile log and namespace are immutable; create a new profile")
			}
			if e := saveProfile(p, update); e != nil {
				return e
			}
			return output(c, map[string]string{"profile": p.Name, "status": "saved"})
		}
		group.AddCommand(c)
	}
	return group
}
