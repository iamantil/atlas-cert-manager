package signer

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"time"

	sampleissuerapi "github.com/cert-manager/sample-external-issuer/api/v1alpha1"
	"github.com/globalsign/hvclient"
)

var err error

type HealthChecker interface {
	Check() error
}

type HealthCheckerBuilder func(*sampleissuerapi.IssuerSpec, map[string][]byte) (HealthChecker, error)

type Signer interface {
	Sign([]byte) ([]byte, error)
}

type SignerBuilder func(*sampleissuerapi.IssuerSpec, map[string][]byte) (Signer, error)

func HVCAHealthCheckerFromIssuerAndSecretData(*sampleissuerapi.IssuerSpec, map[string][]byte) (HealthChecker, error) {
	return &hvcaSigner{}, nil
}

func HVCASignerFromIssuerAndSecretData(spec *sampleissuerapi.IssuerSpec, secret map[string][]byte) (Signer, error) {
	hvconfig := new(hvclient.Config)
	hvconfig.APIKey = string(secret["apikey"])
	hvconfig.APISecret = string(secret["apisecret"])
	hvconfig.URL = string(spec.URL)
	// decode pem to der expected by HVCA signer
	certDER, _ := pem.Decode(secret["cert"])
	keyDER, _ := pem.Decode(secret["certkey"])
	if hvconfig.TLSCert, err = x509.ParseCertificate(certDER.Bytes); err != nil {
		return nil, err
	}
	if hvconfig.TLSKey, err = x509.ParsePKCS1PrivateKey(keyDER.Bytes); err != nil {
		return nil, err
	}
	if err = hvconfig.Validate(); err != nil {
		return nil, err
	}
	return &hvcaSigner{config: hvconfig}, nil
}

type hvcaSigner struct {
	config *hvclient.Config
}

func (o *hvcaSigner) Check() error {
	return nil
}

func (o *hvcaSigner) Sign(csrBytes []byte) ([]byte, error) {
	ctx, cancel := context.WithCancel(context.Background())
	var clnt *hvclient.Client
	var serial *big.Int
	var info *hvclient.CertInfo
	defer cancel()
	if clnt, err = hvclient.NewClient(ctx, o.config); err != nil {
		return nil, err
	}
	// Parse the csr
	csr, err := parseCSR(csrBytes)
	if err != nil {
		return nil, err
	}

	var req = hvclient.Request{
		CSR:      csr,
		Subject:  &hvclient.DN{},
		SAN:      &hvclient.SAN{},
		Validity: &hvclient.Validity{NotBefore: time.Now(), NotAfter: time.Unix(0, 0)},
	}
	// Pull the validation policy and check it for required fields
	vp, err := clnt.Policy(ctx)
	if err != nil {
		return nil, err
	}
	// Subject validation
	// common name
	if vp.SubjectDN.CommonName.Presence == hvclient.Required {
		if csr.Subject.CommonName == "" {
			return nil, errors.New("atlas validation policy requires subject common name, but CSR did not contain one")
		}
		req.Subject.CommonName = csr.Subject.CommonName
	}
	if vp.SubjectDN.CommonName.Presence == hvclient.Optional {
		req.Subject.CommonName = csr.Subject.CommonName
	}

	// serial number
	if vp.SubjectDN.SerialNumber.Presence == hvclient.Required {
		if csr.Subject.SerialNumber == "" {
			return nil, errors.New("atlas validation policy requires subject serial number, but CSR did not contain one")
		}
		req.Subject.SerialNumber = csr.Subject.SerialNumber
	}
	if vp.SubjectDN.SerialNumber.Presence == hvclient.Optional {
		req.Subject.SerialNumber = csr.Subject.SerialNumber
	}
	// Populate SANs
	// DNS Names
	if vp.SAN.DNSNames.Static == false && vp.SAN.DNSNames.MaxCount > 0 {
		if len(csr.DNSNames) < vp.SAN.DNSNames.MaxCount {
			req.SAN.DNSNames = append(req.SAN.DNSNames[:], csr.DNSNames[:]...)
		}
		// Copy the common name into the SAN DNS field if there is space
		if req.Subject.CommonName != "" && len(req.CSR.DNSNames) < vp.SAN.DNSNames.MaxCount {
			req.SAN.DNSNames = append(req.SAN.DNSNames, req.Subject.CommonName)
		}
	}
	// IP addresses
	if vp.SAN.IPAddresses.Static == false && vp.SAN.IPAddresses.MaxCount > 0 {
		if len(csr.IPAddresses) < vp.SAN.IPAddresses.MaxCount {
			req.SAN.IPAddresses = append(req.SAN.IPAddresses[:], csr.IPAddresses[:]...)
		}
	}
	// Validate number of SANs
	if vp.SAN.DNSNames.MinCount > len(req.SAN.DNSNames) || vp.SAN.IPAddresses.MinCount > len(req.SAN.IPAddresses) {
		return nil, errors.New("atlas validation policy requires additional SANs not present in the provided CSR")
	}
	// Check key type
	if vp.PublicKey.KeyType.String() != csr.PublicKeyAlgorithm.String() {
		return nil, errors.New("CSR public key type doesn't match Atlas account pubic key type")
	}
	// Check PKCS type
	if vp.PublicKey.KeyFormat != hvclient.PKCS10 {
		return nil, errors.New("atlas account does not support pkcs10 key format, update atlas account")
	}

	// Request cert
	if serial, err = clnt.CertificateRequest(ctx, &req); err != nil {
		return nil, err
	}
	// Retrieve cert
	if info, err = clnt.CertificateRetrieve(ctx, serial); err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: info.X509.Raw,
	}), nil
}
