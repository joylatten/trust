package main

import (
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/apex/log"
	"github.com/go-git/go-git/v5"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/project-machine/trust/pkg/trust"
	"github.com/urfave/cli"
)

func generateMosCreds(keysetPath string, ctemplate *x509.Certificate) error {
	type AddCertInfo struct {
		cn     string
		doguid bool
	}
	keyinfo := map[string]AddCertInfo{
		"tpmpol-admin":   AddCertInfo{"TPM EAPolicy Admin", false},
		"tpmpol-luks":    AddCertInfo{"TPM EAPolicy LUKS", false},
		"uki-tpm":        AddCertInfo{"UKI TPM", true},
		"uki-limited":    AddCertInfo{"UKI Limited", true},
		"uki-production": AddCertInfo{"UKI Production", true},
		"uefi-db":        AddCertInfo{"UEFI DB", true},
	}

	for key, CertInfo := range keyinfo {
		ctemplate.Subject.CommonName = CertInfo.cn
		err := generateCreds(filepath.Join(keysetPath, key), CertInfo.doguid, ctemplate)
		if err != nil {
			return err
		}
	}
	return nil
}

func makeKeydirs(keysetPath string) error {
	keyDirs := []string{"manifest-ca", "manifest", "sudi-ca", "tpmpol-admin", "tpmpol-luks", "uefi-db", "uki-limited", "uki-production", "uki-tpm", "uefi-pk", "uefi-kek"}
	err := os.MkdirAll(keysetPath, 0750)
	if err != nil {
		return err
	}

	for _, dir := range keyDirs {
		err = os.Mkdir(filepath.Join(keysetPath, dir), 0750)
		if err != nil {
			return err
		}
	}
	return nil
}

func generatebootkit(keysetName, keysetPath, productName string) error {
	cmd := []string{"keysetbootkit.sh", keysetName, keysetPath, productName}
	stdout, stderr, rc := trust.RunCommandWithOutputErrorRc(cmd...)
	if rc != 0 {
		return fmt.Errorf("Failed running keysetbootkit.sh:\nstderr: %s\nstdout: %s\n",
			stderr, stdout)
	}
	return nil
}

func initkeyset(keysetName string, Org []string) error {
	var caTemplate, certTemplate x509.Certificate
	const (
		doGUID = true
		noGUID = false
	)
	if keysetName == "" {
		return errors.New("keyset parameter is missing")
	}

	moskeysetPath, err := getMosKeyPath()
	if err != nil {
		return err
	}
	keysetPath := filepath.Join(moskeysetPath, keysetName)
	if PathExists(keysetPath) {
		return fmt.Errorf("%s keyset already exists", keysetName)
	}

	os.MkdirAll(keysetPath, 0750)

	// Start generating the new keys
	defer func() {
		if err != nil {
			os.RemoveAll(keysetPath)
		}
	}()

	err = makeKeydirs(keysetPath)
	if err != nil {
		return err
	}

	// Prepare certificate template

	//OU := fmt.Sprintf("PuzzlesOS Machine Project %s", keysetName)
	caTemplate.Subject.Organization = Org
	caTemplate.Subject.OrganizationalUnit = []string{"PuzzlesOS Machine Project " + keysetName}
	caTemplate.Subject.CommonName = "Manifest rootCA"
	caTemplate.NotBefore = time.Now()
	caTemplate.NotAfter = time.Now().AddDate(25, 0, 0)
	caTemplate.IsCA = true
	caTemplate.BasicConstraintsValid = true

	// Generate the manifest rootCA
	err = generaterootCA(filepath.Join(keysetPath, "manifest-ca"), &caTemplate, noGUID)
	if err != nil {
		return err
	}

	// Generate the sudi rootCA
	caTemplate.Subject.CommonName = "SUDI rootCA"
	caTemplate.NotAfter = time.Date(2099, time.December, 31, 23, 0, 0, 0, time.UTC)
	err = generaterootCA(filepath.Join(keysetPath, "sudi-ca"), &caTemplate, noGUID)
	if err != nil {
		return err
	}

	// Generate PK
	caTemplate.Subject.CommonName = "UEFI PK"
	caTemplate.NotAfter = time.Now().AddDate(50, 0, 0)
	err = generaterootCA(filepath.Join(keysetPath, "uefi-pk"), &caTemplate, doGUID)
	if err != nil {
		return err
	}

	// Generate additional MOS credentials
	certTemplate.Subject.Organization = Org
	certTemplate.Subject.OrganizationalUnit = []string{"PuzzlesOS Machine Project " + keysetName}
	certTemplate.NotBefore = time.Now()
	certTemplate.NotAfter = time.Now().AddDate(25, 0, 0)
	certTemplate.KeyUsage = x509.KeyUsageDigitalSignature
	certTemplate.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}

	err = generateMosCreds(keysetPath, &certTemplate)
	if err != nil {
		return err
	}

	// Generate KEK, signed by PK
	CAcert, CAprivkey, err := getCA("uefi-pk", keysetName)
	if err != nil {
		return err
	}
	// reuse certTemplate with some modifications
	certTemplate.Subject.CommonName = "UEFI KEK"
	certTemplate.NotAfter = time.Now().AddDate(50, 0, 0)
	certTemplate.ExtKeyUsage = nil
	err = SignCert(&certTemplate, CAcert, CAprivkey, filepath.Join(keysetPath, "uefi-kek"))
	if err != nil {
		return err
	}
	guid := uuid.NewString()
	err = os.WriteFile(filepath.Join(keysetPath, "uefi-kek", "guid"), []byte(guid), 0640)
	if err != nil {
		return err
	}

	// Generate sample uuid, manifest key and cert
	mName := filepath.Join(keysetPath, "manifest", "default")
	if err = trust.EnsureDir(mName); err != nil {
		return errors.Wrapf(err, "Failed creating default project directory")
	}
	sName := filepath.Join(mName, "sudi")
	if err = trust.EnsureDir(sName); err != nil {
		return errors.Wrapf(err, "Failed creating default sudi directory")
	}

	if err = generateNewUUIDCreds(keysetName, mName); err != nil {
		return errors.Wrapf(err, "Failed creating default project keyset")
	}

	// Generate a bootkit for the keyset using keys in the keyset
	if err = generatebootkit(keysetName, keysetPath, "default"); err != nil {
		return errors.Wrapf(err, "Failed to create bootkit artifacts for keyset")
	}

	return nil
}

var keysetCmd = cli.Command{
	Name:  "keyset",
	Usage: "Administer keysets for mos",
	Subcommands: []cli.Command{
		cli.Command{
			Name:   "list",
			Action: doListKeysets,
			Usage:  "list keysets",
		},
		cli.Command{
			Name:      "add",
			Action:    doAddKeyset,
			Usage:     "add a new keyset",
			ArgsUsage: "<keyset-name>",
			Flags: []cli.Flag{
				cli.StringSliceFlag{
					Name:  "org, Org, organization",
					Usage: "X509-Organization field to add to certificates when generating a new keyset. (optional)",
				},
			},
		},
	},
}

func doAddKeyset(ctx *cli.Context) error {
	args := ctx.Args()
	if len(args) != 1 {
		return errors.New("A name for the new keyset is required (please see \"--help\")")
	}

	keysetName := args[0]
	if keysetName == "" {
		return errors.New("Please specify keyset name")
	}

	Org := ctx.StringSlice("org")
	if Org == nil {
		log.Infof("X509-Organization field for new certificates not specified.")
	}

	// See if keyset exists
	mosKeyPath, err := getMosKeyPath()
	if err != nil {
		return err
	}

	keysetPath := filepath.Join(mosKeyPath, keysetName)
	if PathExists(keysetPath) {
		return fmt.Errorf("%s keyset already exists", keysetName)
	}

	// git clone if keyset is snakeoil
	if keysetName == "snakeoil" {
		_, err = git.PlainClone(keysetPath, false, &git.CloneOptions{URL: "https://github.com/project-machine/keys.git"})
		if err != nil {
			os.Remove(keysetPath)
			return err
		}
		return nil
	}
	// Otherwise, generate a new keyset
	return initkeyset(keysetName, Org)
}

func doListKeysets(ctx *cli.Context) error {
	if len(ctx.Args()) != 0 {
		return fmt.Errorf("Wrong number of arguments (please see \"--help\")")
	}
	moskeysetPath, err := getMosKeyPath()
	if err != nil {
		return err
	}
	dirs,  err := os.ReadDir(moskeysetPath)
	if err != nil {
		return fmt.Errorf("Failed reading keys directory %q: %w", moskeysetPath, err)
	}

	for _, keyname := range dirs {
		fmt.Printf("%s\n", keyname.Name())
	}

	return nil
}
