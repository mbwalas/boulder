// Copyright 2014 ISRG.  All rights reserved
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package ca

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"sort"
	"testing"
	"time"

	cfsslConfig "github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/cloudflare/cfssl/config"
	"github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/cloudflare/cfssl/helpers"
	ocspConfig "github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/cloudflare/cfssl/ocsp/config"
	"github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/jmhodges/clock"
	"github.com/letsencrypt/boulder/cmd"
	"github.com/letsencrypt/boulder/mocks"
	"github.com/letsencrypt/boulder/policy"
	"github.com/letsencrypt/boulder/sa/satest"

	"github.com/letsencrypt/boulder/core"
	"github.com/letsencrypt/boulder/sa"
	"github.com/letsencrypt/boulder/test"
	"github.com/letsencrypt/boulder/test/vars"
)

var (
	CAkeyPEM  = mustRead("./testdata/ca_key.pem")
	CAcertPEM = mustRead("./testdata/ca_cert.pem")

	// CSR generated by Go:
	// * Random public key
	// * CN = not-example.com
	// * DNSNames = not-example.com, www.not-example.com
	CNandSANCSR = mustRead("./testdata/cn_and_san.der.csr")

	// CSR generated by Go:
	// * Random public key
	// * CN = not-example.com
	// * DNSNames = [none]
	NoSANCSR = mustRead("./testdata/no_san.der.csr")

	// CSR generated by Go:
	// * Random public key
	// * C = US
	// * CN = [none]
	// * DNSNames = not-example.com
	NoCNCSR = mustRead("./testdata/no_cn.der.csr")

	// CSR generated by Go:
	// * Random public key
	// * C = US
	// * CN = [none]
	// * DNSNames = [none]
	NoNameCSR = mustRead("./testdata/no_name.der.csr")

	// CSR generated by Go:
	// * Random public key
	// * CN = [none]
	// * DNSNames = a.example.com, a.example.com
	DupeNameCSR = mustRead("./testdata/dupe_name.der.csr")

	// CSR generated by Go:
	// * Random public key
	// * CN = [none]
	// * DNSNames = not-example.com, www.not-example.com, mail.example.com
	TooManyNameCSR = mustRead("./testdata/too_many_names.der.csr")

	// CSR generated by Go:
	// * Random public key -- 512 bits long
	// * CN = (none)
	// * DNSNames = not-example.com, www.not-example.com, mail.not-example.com
	ShortKeyCSR = mustRead("./testdata/short_key.der.csr")

	// CSR generated by Go:
	// * Random public key
	// * CN = (none)
	// * DNSNames = not-example.com, www.not-example.com, mail.not-example.com
	// * Signature algorithm: SHA1WithRSA
	BadAlgorithmCSR = mustRead("./testdata/bad_algorithm.der.csr")

	// CSR generated by Go:
	// * Random public key
	// * CN = CapiTalizedLetters.com
	// * DNSNames = moreCAPs.com, morecaps.com, evenMOREcaps.com, Capitalizedletters.COM
	CapitalizedCSR = mustRead("./testdata/capitalized_cn_and_san.der.csr")

	// CSR generated by Go:
	// * Random public key
	// * CN = not-example.com
	// * Includes an extensionRequest attribute for a well-formed TLS Feature extension
	MustStapleCSR = mustRead("./testdata/must_staple.der.csr")

	// CSR generated by Go:
	// * Random public key
	// * CN = not-example.com
	// * Includes an extensionRequest attribute for an empty TLS Feature extension
	TLSFeatureUnknownCSR = mustRead("./testdata/tls_feature_unknown.der.csr")

	// CSR generated by Go:
	// * Random public key
	// * CN = not-example.com
	// * Includes an extensionRequest attribute for the CT Poison extension (not supported)
	UnsupportedExtensionCSR = mustRead("./testdata/unsupported_extension.der.csr")

	log = mocks.UseMockLog()
)

// CFSSL config
const profileName = "ee"
const caKeyFile = "../test/test-ca.key"
const caCertFile = "../test/test-ca.pem"

func mustRead(path string) []byte {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("unable to read %#v: %s", path, err))
	}
	return b
}

type testCtx struct {
	sa       core.StorageAuthority
	caConfig cmd.CAConfig
	reg      core.Registration
	pa       core.PolicyAuthority
	fc       clock.FakeClock
	stats    *mocks.Statter
	cleanUp  func()
}

var caKey crypto.Signer
var caCert *x509.Certificate

func init() {
	var err error
	caKey, err = helpers.ParsePrivateKeyPEM(mustRead(caKeyFile))
	if err != nil {
		panic(fmt.Sprintf("Unable to parse %s: %s", caKeyFile, err))
	}
	caCert, err = core.LoadCert(caCertFile)
	if err != nil {
		panic(fmt.Sprintf("Unable to parse %s: %s", caCertFile, err))
	}
}

func setup(t *testing.T) *testCtx {
	// Create an SA
	dbMap, err := sa.NewDbMap(vars.DBConnSA)
	if err != nil {
		t.Fatalf("Failed to create dbMap: %s", err)
	}
	fc := clock.NewFake()
	fc.Add(1 * time.Hour)
	ssa, err := sa.NewSQLStorageAuthority(dbMap, fc)
	if err != nil {
		t.Fatalf("Failed to create SA: %s", err)
	}
	saDBCleanUp := test.ResetSATestDatabase(t)

	paDbMap, err := sa.NewDbMap(vars.DBConnPolicy)
	test.AssertNotError(t, err, "Could not construct dbMap")
	pa, err := policy.NewPolicyAuthorityImpl(paDbMap, false, nil)
	test.AssertNotError(t, err, "Couldn't create PADB")
	paDBCleanUp := test.ResetPolicyTestDatabase(t)

	cleanUp := func() {
		saDBCleanUp()
		paDBCleanUp()
	}

	// TODO(jmhodges): use of this pkg here is a bug caused by using a real SA
	reg := satest.CreateWorkingRegistration(t, ssa)

	// Create a CA
	caConfig := cmd.CAConfig{
		Profile:         profileName,
		SerialPrefix:    17,
		Expiry:          "8760h",
		LifespanOCSP:    "45m",
		MaxNames:        2,
		HSMFaultTimeout: cmd.ConfigDuration{Duration: 60 * time.Second},
		CFSSL: cfsslConfig.Config{
			Signing: &cfsslConfig.Signing{
				Profiles: map[string]*cfsslConfig.SigningProfile{
					profileName: &cfsslConfig.SigningProfile{
						Usage:     []string{"server auth"},
						CA:        false,
						IssuerURL: []string{"http://not-example.com/issuer-url"},
						OCSP:      "http://not-example.com/ocsp",
						CRL:       "http://not-example.com/crl",

						Policies: []cfsslConfig.CertificatePolicy{
							cfsslConfig.CertificatePolicy{
								ID: cfsslConfig.OID(asn1.ObjectIdentifier{2, 23, 140, 1, 2, 1}),
							},
						},
						ExpiryString: "8760h",
						Backdate:     time.Hour,
						CSRWhitelist: &cfsslConfig.CSRWhitelist{
							PublicKeyAlgorithm: true,
							PublicKey:          true,
							SignatureAlgorithm: true,
						},
						AllowedExtensions: []cfsslConfig.OID{
							cfsslConfig.OID(oidTLSFeature),
						},
					},
				},
				Default: &cfsslConfig.SigningProfile{
					ExpiryString: "8760h",
				},
			},
			OCSP: &ocspConfig.Config{
				CACertFile:        caCertFile,
				ResponderCertFile: caCertFile,
				KeyFile:           caKeyFile,
			},
		},
	}

	stats := mocks.NewStatter()

	return &testCtx{
		ssa,
		caConfig,
		reg,
		pa,
		fc,
		&stats,
		cleanUp,
	}
}

func TestFailNoSerial(t *testing.T) {
	ctx := setup(t)
	defer ctx.cleanUp()

	ctx.caConfig.SerialPrefix = 0
	_, err := NewCertificateAuthorityImpl(ctx.caConfig, ctx.fc, ctx.stats, caCert, caKey)
	test.AssertError(t, err, "CA should have failed with no SerialPrefix")
}

func TestIssueCertificate(t *testing.T) {
	ctx := setup(t)
	defer ctx.cleanUp()
	ca, err := NewCertificateAuthorityImpl(ctx.caConfig, ctx.fc, ctx.stats, caCert, caKey)
	test.AssertNotError(t, err, "Failed to create CA")
	ca.Publisher = &mocks.Publisher{}
	ca.PA = ctx.pa
	ca.SA = ctx.sa

	/*
		  // Uncomment to test with a local signer
			signer, _ := local.NewSigner(caKey, caCert, x509.SHA256WithRSA, nil)
			ca := CertificateAuthorityImpl{
				Signer: signer,
				SA:     sa,
			}
	*/

	csrs := [][]byte{CNandSANCSR, NoSANCSR, NoCNCSR}
	for _, csrDER := range csrs {
		csr, _ := x509.ParseCertificateRequest(csrDER)

		// Sign CSR
		issuedCert, err := ca.IssueCertificate(*csr, ctx.reg.ID)
		test.AssertNotError(t, err, "Failed to sign certificate")
		if err != nil {
			continue
		}

		// Verify cert contents
		cert, err := x509.ParseCertificate(issuedCert.DER)
		test.AssertNotError(t, err, "Certificate failed to parse")

		test.AssertEquals(t, cert.Subject.CommonName, "not-example.com")

		switch len(cert.DNSNames) {
		case 1:
			if cert.DNSNames[0] != "not-example.com" {
				t.Errorf("Improper list of domain names %v", cert.DNSNames)
			}
		case 2:
			switch {
			case (cert.DNSNames[0] == "not-example.com" && cert.DNSNames[1] == "www.not-example.com"):
				t.Log("case 1")
			case (cert.DNSNames[0] == "www.not-example.com" && cert.DNSNames[1] == "not-example.com"):
				t.Log("case 2")
			default:
				t.Errorf("Improper list of domain names %v", cert.DNSNames)
			}

		default:
			t.Errorf("Improper list of domain names %v", cert.DNSNames)
		}

		// Test is broken by CFSSL Issue #156
		// https://github.com/cloudflare/cfssl/issues/156
		if len(cert.Subject.Country) > 0 {
			// Uncomment the Errorf as soon as upstream #156 is fixed
			// t.Errorf("Subject contained unauthorized values: %v", cert.Subject)
			t.Logf("Subject contained unauthorized values: %v", cert.Subject)
		}

		// Verify that the cert got stored in the DB
		serialString := core.SerialToString(cert.SerialNumber)
		storedCert, err := ctx.sa.GetCertificate(serialString)
		test.AssertNotError(t, err,
			fmt.Sprintf("Certificate %s not found in database", serialString))
		test.Assert(t, bytes.Equal(issuedCert.DER, storedCert.DER), "Retrieved cert not equal to issued cert.")

		certStatus, err := ctx.sa.GetCertificateStatus(serialString)
		test.AssertNotError(t, err,
			fmt.Sprintf("Error fetching status for certificate %s", serialString))
		test.Assert(t, certStatus.Status == core.OCSPStatusGood, "Certificate status was not good")
		test.Assert(t, certStatus.SubscriberApproved == false, "Subscriber shouldn't have approved cert yet.")
	}
}

func TestRejectNoName(t *testing.T) {
	ctx := setup(t)
	defer ctx.cleanUp()
	ca, err := NewCertificateAuthorityImpl(ctx.caConfig, ctx.fc, ctx.stats, caCert, caKey)
	test.AssertNotError(t, err, "Failed to create CA")
	ca.Publisher = &mocks.Publisher{}
	ca.PA = ctx.pa
	ca.SA = ctx.sa

	// Test that the CA rejects CSRs with no names
	csr, _ := x509.ParseCertificateRequest(NoNameCSR)
	_, err = ca.IssueCertificate(*csr, ctx.reg.ID)
	test.AssertError(t, err, "CA improperly agreed to create a certificate with no name")
	_, ok := err.(core.MalformedRequestError)
	test.Assert(t, ok, "Incorrect error type returned")
}

func TestRejectTooManyNames(t *testing.T) {
	ctx := setup(t)
	defer ctx.cleanUp()
	ca, err := NewCertificateAuthorityImpl(ctx.caConfig, ctx.fc, ctx.stats, caCert, caKey)
	test.AssertNotError(t, err, "Failed to create CA")
	ca.Publisher = &mocks.Publisher{}
	ca.PA = ctx.pa
	ca.SA = ctx.sa

	// Test that the CA rejects a CSR with too many names
	csr, _ := x509.ParseCertificateRequest(TooManyNameCSR)
	_, err = ca.IssueCertificate(*csr, ctx.reg.ID)
	test.AssertError(t, err, "Issued certificate with too many names")
	_, ok := err.(core.MalformedRequestError)
	test.Assert(t, ok, "Incorrect error type returned")
}

func TestDeduplication(t *testing.T) {
	ctx := setup(t)
	defer ctx.cleanUp()
	ca, err := NewCertificateAuthorityImpl(ctx.caConfig, ctx.fc, ctx.stats, caCert, caKey)
	test.AssertNotError(t, err, "Failed to create CA")
	ca.Publisher = &mocks.Publisher{}
	ca.PA = ctx.pa
	ca.SA = ctx.sa

	// Test that the CA collapses duplicate names
	csr, _ := x509.ParseCertificateRequest(DupeNameCSR)
	cert, err := ca.IssueCertificate(*csr, ctx.reg.ID)
	test.AssertNotError(t, err, "Failed to gracefully handle a CSR with duplicate names")

	parsedCert, err := x509.ParseCertificate(cert.DER)
	test.AssertNotError(t, err, "Error parsing certificate produced by CA")

	correctName := "a.not-example.com"
	correctNames := len(parsedCert.DNSNames) == 1 &&
		parsedCert.DNSNames[0] == correctName &&
		parsedCert.Subject.CommonName == correctName
	test.Assert(t, correctNames, "Incorrect set of names in deduplicated certificate")
}

func TestRejectValidityTooLong(t *testing.T) {
	ctx := setup(t)
	defer ctx.cleanUp()
	ca, err := NewCertificateAuthorityImpl(ctx.caConfig, ctx.fc, ctx.stats, caCert, caKey)
	test.AssertNotError(t, err, "Failed to create CA")
	ca.Publisher = &mocks.Publisher{}
	ca.PA = ctx.pa
	ca.SA = ctx.sa

	// Test that the CA rejects CSRs that would expire after the intermediate cert
	csr, _ := x509.ParseCertificateRequest(NoCNCSR)
	ca.notAfter = ctx.fc.Now()
	_, err = ca.IssueCertificate(*csr, 1)
	test.AssertEquals(t, err.Error(), "Cannot issue a certificate that expires after the intermediate certificate.")
	_, ok := err.(core.InternalServerError)
	test.Assert(t, ok, "Incorrect error type returned")
}

func TestShortKey(t *testing.T) {
	ctx := setup(t)
	defer ctx.cleanUp()
	ca, err := NewCertificateAuthorityImpl(ctx.caConfig, ctx.fc, ctx.stats, caCert, caKey)
	ca.Publisher = &mocks.Publisher{}
	ca.PA = ctx.pa
	ca.SA = ctx.sa

	// Test that the CA rejects CSRs that would expire after the intermediate cert
	csr, _ := x509.ParseCertificateRequest(ShortKeyCSR)
	_, err = ca.IssueCertificate(*csr, ctx.reg.ID)
	test.AssertError(t, err, "Issued a certificate with too short a key.")
	_, ok := err.(core.MalformedRequestError)
	test.Assert(t, ok, "Incorrect error type returned")
}

func TestRejectBadAlgorithm(t *testing.T) {
	ctx := setup(t)
	defer ctx.cleanUp()
	ca, err := NewCertificateAuthorityImpl(ctx.caConfig, ctx.fc, ctx.stats, caCert, caKey)
	ca.Publisher = &mocks.Publisher{}
	ca.PA = ctx.pa
	ca.SA = ctx.sa

	// Test that the CA rejects CSRs that would expire after the intermediate cert
	csr, _ := x509.ParseCertificateRequest(BadAlgorithmCSR)
	_, err = ca.IssueCertificate(*csr, ctx.reg.ID)
	test.AssertError(t, err, "Issued a certificate based on a CSR with a weak algorithm.")
	_, ok := err.(core.MalformedRequestError)
	test.Assert(t, ok, "Incorrect error type returned")
}

func TestCapitalizedLetters(t *testing.T) {
	ctx := setup(t)
	defer ctx.cleanUp()
	ctx.caConfig.MaxNames = 3
	ca, err := NewCertificateAuthorityImpl(ctx.caConfig, ctx.fc, ctx.stats, caCert, caKey)
	ca.Publisher = &mocks.Publisher{}
	ca.PA = ctx.pa
	ca.SA = ctx.sa

	csr, _ := x509.ParseCertificateRequest(CapitalizedCSR)
	cert, err := ca.IssueCertificate(*csr, ctx.reg.ID)
	test.AssertNotError(t, err, "Failed to gracefully handle a CSR with capitalized names")

	parsedCert, err := x509.ParseCertificate(cert.DER)
	test.AssertNotError(t, err, "Error parsing certificate produced by CA")
	test.AssertEquals(t, "capitalizedletters.com", parsedCert.Subject.CommonName)
	sort.Strings(parsedCert.DNSNames)
	expected := []string{"capitalizedletters.com", "evenmorecaps.com", "morecaps.com"}
	test.AssertDeepEquals(t, expected, parsedCert.DNSNames)
}

func TestHSMFaultTimeout(t *testing.T) {
	ctx := setup(t)
	defer ctx.cleanUp()

	ca, err := NewCertificateAuthorityImpl(ctx.caConfig, ctx.fc, ctx.stats, caCert, caKey)
	ca.Publisher = &mocks.Publisher{}
	ca.PA = ctx.pa
	ca.SA = ctx.sa

	// Issue a certificate so that we can use it later
	csr, _ := x509.ParseCertificateRequest(CNandSANCSR)
	cert, err := ca.IssueCertificate(*csr, ctx.reg.ID)
	ocspRequest := core.OCSPSigningRequest{
		CertDER: cert.DER,
		Status:  "good",
	}

	// Swap in a bad signer
	goodSigner := ca.signer
	badHSMErrorMessage := "This is really serious.  You should wait"
	badSigner := mocks.BadHSMSigner(badHSMErrorMessage)
	badOCSPSigner := mocks.BadHSMOCSPSigner(badHSMErrorMessage)

	// Cause the CA to enter the HSM fault condition
	ca.signer = badSigner
	_, err = ca.IssueCertificate(*csr, ctx.reg.ID)
	test.AssertError(t, err, "CA failed to return HSM error")
	test.AssertEquals(t, err.Error(), badHSMErrorMessage)

	// Check that the CA rejects the next call as the HSM being down
	_, err = ca.IssueCertificate(*csr, ctx.reg.ID)
	test.AssertError(t, err, "CA failed to persist HSM fault")
	test.AssertEquals(t, err.Error(), "HSM is unavailable")

	_, err = ca.GenerateOCSP(ocspRequest)
	test.AssertError(t, err, "CA failed to persist HSM fault")
	test.AssertEquals(t, err.Error(), "HSM is unavailable")

	// Swap in a good signer and move the clock forward to clear the fault
	ca.signer = goodSigner
	ctx.fc.Add(ca.hsmFaultTimeout)
	ctx.fc.Add(10 * time.Second)

	// Check that the CA has recovered
	_, err = ca.IssueCertificate(*csr, ctx.reg.ID)
	test.AssertNotError(t, err, "CA failed to recover from HSM fault")
	_, err = ca.GenerateOCSP(ocspRequest)

	// Check that GenerateOCSP can also trigger an HSM failure, in the same way
	ca.ocspSigner = badOCSPSigner
	_, err = ca.GenerateOCSP(ocspRequest)
	test.AssertError(t, err, "CA failed to return HSM error")
	test.AssertEquals(t, err.Error(), badHSMErrorMessage)

	_, err = ca.IssueCertificate(*csr, ctx.reg.ID)
	test.AssertError(t, err, "CA failed to persist HSM fault")
	test.AssertEquals(t, err.Error(), "HSM is unavailable")

	_, err = ca.GenerateOCSP(ocspRequest)
	test.AssertError(t, err, "CA failed to persist HSM fault")
	test.AssertEquals(t, err.Error(), "HSM is unavailable")

	// Verify that the appropriate stats got recorded for all this
	test.AssertEquals(t, ctx.stats.Counters[metricHSMFaultObserved], int64(2))
	test.AssertEquals(t, ctx.stats.Counters[metricHSMFaultRejected], int64(4))
}

func TestExtensions(t *testing.T) {
	ctx := setup(t)
	defer ctx.cleanUp()
	ctx.caConfig.MaxNames = 3
	ca, err := NewCertificateAuthorityImpl(ctx.caConfig, ctx.fc, ctx.stats, caCertFile)
	ca.Publisher = &mocks.Publisher{}
	ca.PA = ctx.pa
	ca.SA = ctx.sa

	// A TLS feature extension should put a must-staple extension into the cert
	csr, _ := x509.ParseCertificateRequest(MustStapleCSR)
	cert, err := ca.IssueCertificate(*csr, ctx.reg.ID)
	test.AssertNotError(t, err, "Failed to gracefully handle a CSR with must_staple")
	parsedCert, err := x509.ParseCertificate(cert.DER)
	test.AssertNotError(t, err, "Error parsing certificate produced by CA")

	// We already expect 8 extensions: extKeyUsage, basicConstraints,
	// subjectKeyIdentifier, authorityKeyIdentifier, subjectAlternativeName,
	// crlDistributionPoints, authorityInfoAccess, certificatePolicies
	test.AssertEquals(t, len(parsedCert.Extensions), 9)
	foundMustStaple := false
	for _, ext := range parsedCert.Extensions {
		if ext.Id.Equal(oidTLSFeature) {
			foundMustStaple = true
			test.Assert(t, !ext.Critical, "Extension was marked critical")
			test.AssertEquals(t, hex.EncodeToString(ext.Value), mustStapleFeatureValue)
		}
	}
	test.Assert(t, foundMustStaple, "TLS Feature extension not found")

	// ... even if it doesn't ask for stapling
	csr, _ = x509.ParseCertificateRequest(TLSFeatureUnknownCSR)
	cert, err = ca.IssueCertificate(*csr, ctx.reg.ID)
	test.AssertNotError(t, err, "Failed to gracefully handle a CSR with an empty TLS feature extension")
	parsedCert, err = x509.ParseCertificate(cert.DER)
	test.AssertNotError(t, err, "Error parsing certificate produced by CA")

	test.AssertEquals(t, len(parsedCert.Extensions), 9)
	foundMustStaple = false
	for _, ext := range parsedCert.Extensions {
		if ext.Id.Equal(oidTLSFeature) {
			foundMustStaple = true
			test.Assert(t, !ext.Critical, "Extension was marked critical")
			test.AssertEquals(t, hex.EncodeToString(ext.Value), mustStapleFeatureValue)
		}
	}
	test.Assert(t, foundMustStaple, "TLS Feature extension not found")

	// Unsupported extensions should be ignored
	csr, _ = x509.ParseCertificateRequest(UnsupportedExtensionCSR)
	cert, err = ca.IssueCertificate(*csr, ctx.reg.ID)
	test.AssertNotError(t, err, "Failed to gracefully handle a CSR with an unsupported extension")
	parsedCert, err = x509.ParseCertificate(cert.DER)
	test.AssertNotError(t, err, "Error parsing certificate produced by CA")
	test.AssertEquals(t, len(parsedCert.Extensions), 8)
}
