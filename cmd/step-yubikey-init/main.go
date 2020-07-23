package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/pkg/errors"
	"github.com/smallstep/certificates/kms"
	"github.com/smallstep/certificates/kms/apiv1"
	"github.com/smallstep/cli/crypto/pemutil"
	"github.com/smallstep/cli/ui"
	"github.com/smallstep/cli/utils"

	// Enable yubikey.
	_ "github.com/smallstep/certificates/kms/yubikey"
)

type Config struct {
	RootOnly bool
	RootSlot string
	CrtSlot  string
	RootFile string
	KeyFile  string
	Pin      string
	Force    bool
}

func (c *Config) Validate() error {
	switch {
	case c.RootFile != "" && c.KeyFile == "":
		return errors.New("flag `--root` requires flag `--key`")
	case c.KeyFile != "" && c.RootFile == "":
		return errors.New("flag `--key` requires flag `--root`")
	case c.RootOnly && c.RootFile != "":
		return errors.New("flag `--root-only` is incompatible with flag `--root`")
	case c.RootSlot == c.CrtSlot:
		return errors.New("flag `--root-slot` and flag `--crt-slot` cannot be the same")
	case c.RootFile == "" && c.RootSlot == "":
		return errors.New("one of flag `--root` or `--root-slot` is required")
	default:
		if c.RootFile != "" {
			c.RootSlot = ""
		}
		if c.RootOnly {
			c.CrtSlot = ""
		}
		return nil
	}
}

func main() {
	var c Config
	flag.BoolVar(&c.RootOnly, "root-only", false, "Slot only the root certificate and sign and intermediate.")
	flag.StringVar(&c.RootSlot, "root-slot", "9a", "Slot to store the root certificate.")
	flag.StringVar(&c.CrtSlot, "crt-slot", "9c", "Slot to store the intermediate certificate.")
	flag.StringVar(&c.RootFile, "root", "", "Path to the root certificate to use.")
	flag.StringVar(&c.KeyFile, "key", "", "Path to the root key to use.")
	flag.BoolVar(&c.Force, "force", false, "Force the delete of previous keys.")
	flag.Usage = usage
	flag.Parse()

	if err := c.Validate(); err != nil {
		fatal(err)
	}

	pin, err := ui.PromptPassword("What is the YubiKey PIN?")
	if err != nil {
		fatal(err)
	}
	c.Pin = string(pin)

	k, err := kms.New(context.Background(), apiv1.Options{
		Type: string(apiv1.YubiKey),
		Pin:  c.Pin,
	})
	if err != nil {
		fatal(err)
	}

	// Check if the slots are empty, fail if they are not
	if !c.Force {
		switch {
		case c.RootSlot != "":
			checkSlot(k, c.RootSlot)
		case c.CrtSlot != "":
			checkSlot(k, c.CrtSlot)
		}
	}

	if err := createPKI(k, c); err != nil {
		fatal(err)
	}

	defer func() {
		_ = k.Close()
	}()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: step-yubikey-init")
	fmt.Fprintln(os.Stderr, `
The step-yubikey-init command initializes a public key infrastructure (PKI)
to be used by step-ca.

This tool is experimental and in the future it will be integrated in step cli.

OPTIONS`)
	fmt.Fprintln(os.Stderr)
	flag.PrintDefaults()
	fmt.Fprintln(os.Stderr, `
COPYRIGHT

  (c) 2018-2020 Smallstep Labs, Inc.`)
	os.Exit(1)
}

func checkSlot(k kms.KeyManager, slot string) {
	if _, err := k.GetPublicKey(&apiv1.GetPublicKeyRequest{
		Name: slot,
	}); err == nil {
		fmt.Fprintf(os.Stderr, "⚠️  Your YubiKey already has a key in the slot %s.\n", slot)
		fmt.Fprintln(os.Stderr, "   If you want to delete it and start fresh, use `--force`.")
		os.Exit(1)
	}
}

func createPKI(k kms.KeyManager, c Config) error {
	var err error
	ui.Println("Creating PKI ...")
	now := time.Now()

	// Root Certificate
	var signer crypto.Signer
	var root *x509.Certificate
	if c.RootFile != "" && c.KeyFile != "" {
		root, err = pemutil.ReadCertificate(c.RootFile)
		if err != nil {
			return err
		}

		key, err := pemutil.Read(c.KeyFile)
		if err != nil {
			return err
		}

		var ok bool
		if signer, ok = key.(crypto.Signer); !ok {
			return errors.Errorf("key type '%T' does not implement a signer", key)
		}
	} else {
		resp, err := k.CreateKey(&apiv1.CreateKeyRequest{
			Name:               c.RootSlot,
			SignatureAlgorithm: apiv1.ECDSAWithSHA256,
		})
		if err != nil {
			return err
		}

		signer, err = k.CreateSigner(&resp.CreateSignerRequest)
		if err != nil {
			return err
		}

		template := &x509.Certificate{
			IsCA:                  true,
			NotBefore:             now,
			NotAfter:              now.Add(time.Hour * 24 * 365 * 10),
			KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
			BasicConstraintsValid: true,
			MaxPathLen:            1,
			MaxPathLenZero:        false,
			Issuer:                pkix.Name{CommonName: "YubiKey Smallstep Root"},
			Subject:               pkix.Name{CommonName: "YubiKey Smallstep Root"},
			SerialNumber:          mustSerialNumber(),
			SubjectKeyId:          mustSubjectKeyID(resp.PublicKey),
			AuthorityKeyId:        mustSubjectKeyID(resp.PublicKey),
		}

		b, err := x509.CreateCertificate(rand.Reader, template, template, resp.PublicKey, signer)
		if err != nil {
			return err
		}

		root, err = x509.ParseCertificate(b)
		if err != nil {
			return errors.Wrap(err, "error parsing root certificate")
		}

		if cm, ok := k.(kms.CertificateManager); ok {
			if err = cm.StoreCertificate(&apiv1.StoreCertificateRequest{
				Name:        c.RootSlot,
				Certificate: root,
			}); err != nil {
				return err
			}
		}

		if err = utils.WriteFile("root_ca.crt", pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: b,
		}), 0600); err != nil {
			return err
		}

		ui.PrintSelected("Root Key", resp.Name)
		ui.PrintSelected("Root Certificate", "root_ca.crt")
	}

	// Intermediate Certificate
	var keyName string
	var publicKey crypto.PublicKey
	if c.RootOnly {
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return errors.Wrap(err, "error creating intermediate key")
		}

		pass, err := ui.PromptPasswordGenerate("What do you want your password to be? [leave empty and we'll generate one]",
			ui.WithRichPrompt())
		if err != nil {
			return err
		}

		_, err = pemutil.Serialize(priv, pemutil.WithPassword(pass), pemutil.ToFile("intermediate_ca_key", 0600))
		if err != nil {
			return err
		}

		publicKey = priv.Public()
	} else {
		resp, err := k.CreateKey(&apiv1.CreateKeyRequest{
			Name:               c.CrtSlot,
			SignatureAlgorithm: apiv1.ECDSAWithSHA256,
		})
		if err != nil {
			return err
		}
		publicKey = resp.PublicKey
		keyName = resp.Name
	}

	template := &x509.Certificate{
		IsCA:                  true,
		NotBefore:             now,
		NotAfter:              now.Add(time.Hour * 24 * 365 * 10),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		Issuer:                root.Subject,
		Subject:               pkix.Name{CommonName: "YubiKey Smallstep Intermediate"},
		SerialNumber:          mustSerialNumber(),
		SubjectKeyId:          mustSubjectKeyID(publicKey),
	}

	b, err := x509.CreateCertificate(rand.Reader, template, root, publicKey, signer)
	if err != nil {
		return err
	}

	intermediate, err := x509.ParseCertificate(b)
	if err != nil {
		return errors.Wrap(err, "error parsing intermediate certificate")
	}

	if cm, ok := k.(kms.CertificateManager); ok {
		if err = cm.StoreCertificate(&apiv1.StoreCertificateRequest{
			Name:        c.CrtSlot,
			Certificate: intermediate,
		}); err != nil {
			return err
		}
	}

	if err = utils.WriteFile("intermediate_ca.crt", pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: b,
	}), 0600); err != nil {
		return err
	}

	if c.RootOnly {
		ui.PrintSelected("Intermediate Key", "intermediate_ca_key")
	} else {
		ui.PrintSelected("Intermediate Key", keyName)
	}

	ui.PrintSelected("Intermediate Certificate", "intermediate_ca.crt")

	return nil
}

func mustSerialNumber() *big.Int {
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	sn, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		panic(err)
	}
	return sn
}

func mustSubjectKeyID(key crypto.PublicKey) []byte {
	b, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		panic(err)
	}
	hash := sha1.Sum(b)
	return hash[:]
}
