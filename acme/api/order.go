package api

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi"

	"go.step.sm/crypto/randutil"

	"github.com/smallstep/certificates/acme"
	"github.com/smallstep/certificates/api/render"
)

// NewOrderRequest represents the body for a NewOrder request.
type NewOrderRequest struct {
	Identifiers []acme.Identifier `json:"identifiers"`
	NotBefore   time.Time         `json:"notBefore,omitempty"`
	NotAfter    time.Time         `json:"notAfter,omitempty"`
}

// Validate validates a new-order request body.
func (n *NewOrderRequest) Validate() error {
	if len(n.Identifiers) == 0 {
		return acme.NewError(acme.ErrorMalformedType, "identifiers list cannot be empty")
	}
	for _, id := range n.Identifiers {
		if !(id.Type == acme.DNS || id.Type == acme.IP) {
			return acme.NewError(acme.ErrorMalformedType, "identifier type unsupported: %s", id.Type)
		}
		if id.Type == acme.IP && net.ParseIP(id.Value) == nil {
			return acme.NewError(acme.ErrorMalformedType, "invalid IP address: %s", id.Value)
		}
	}
	return nil
}

// FinalizeRequest captures the body for a Finalize order request.
type FinalizeRequest struct {
	CSR string `json:"csr"`
	csr *x509.CertificateRequest
}

// Validate validates a finalize request body.
func (f *FinalizeRequest) Validate() error {
	var err error
	csrBytes, err := base64.RawURLEncoding.DecodeString(f.CSR)
	if err != nil {
		return acme.WrapError(acme.ErrorMalformedType, err, "error base64url decoding csr")
	}
	f.csr, err = x509.ParseCertificateRequest(csrBytes)
	if err != nil {
		return acme.WrapError(acme.ErrorMalformedType, err, "unable to parse csr")
	}
	if err = f.csr.CheckSignature(); err != nil {
		return acme.WrapError(acme.ErrorMalformedType, err, "csr failed signature check")
	}
	return nil
}

var defaultOrderExpiry = time.Hour * 24
var defaultOrderBackdate = time.Minute

// NewOrder ACME api for creating a new order.
func NewOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	db := acme.MustDatabaseFromContext(ctx)
	linker := acme.MustLinkerFromContext(ctx)

	acc, err := accountFromContext(ctx)
	if err != nil {
		render.Error(w, err)
		return
	}
	prov, err := provisionerFromContext(ctx)
	if err != nil {
		render.Error(w, err)
		return
	}
	payload, err := payloadFromContext(ctx)
	if err != nil {
		render.Error(w, err)
		return
	}
	var nor NewOrderRequest
	if err := json.Unmarshal(payload.value, &nor); err != nil {
		render.Error(w, acme.WrapError(acme.ErrorMalformedType, err,
			"failed to unmarshal new-order request payload"))
		return
	}

	if err := nor.Validate(); err != nil {
		render.Error(w, err)
		return
	}

	now := clock.Now()
	// New order.
	o := &acme.Order{
		AccountID:        acc.ID,
		ProvisionerID:    prov.GetID(),
		Status:           acme.StatusPending,
		Identifiers:      nor.Identifiers,
		ExpiresAt:        now.Add(defaultOrderExpiry),
		AuthorizationIDs: make([]string, len(nor.Identifiers)),
		NotBefore:        nor.NotBefore,
		NotAfter:         nor.NotAfter,
	}

	for i, identifier := range o.Identifiers {
		az := &acme.Authorization{
			AccountID:  acc.ID,
			Identifier: identifier,
			ExpiresAt:  o.ExpiresAt,
			Status:     acme.StatusPending,
		}
		if err := newAuthorization(ctx, az); err != nil {
			render.Error(w, err)
			return
		}
		o.AuthorizationIDs[i] = az.ID
	}

	if o.NotBefore.IsZero() {
		o.NotBefore = now
	}
	if o.NotAfter.IsZero() {
		o.NotAfter = o.NotBefore.Add(prov.DefaultTLSCertDuration())
	}
	// If request NotBefore was empty then backdate the order.NotBefore (now)
	// to avoid timing issues.
	if nor.NotBefore.IsZero() {
		o.NotBefore = o.NotBefore.Add(-defaultOrderBackdate)
	}

	if err := db.CreateOrder(ctx, o); err != nil {
		render.Error(w, acme.WrapErrorISE(err, "error creating order"))
		return
	}

	linker.LinkOrder(ctx, o)

	w.Header().Set("Location", linker.GetLink(ctx, acme.OrderLinkType, o.ID))
	render.JSONStatus(w, o, http.StatusCreated)
}

func newAuthorization(ctx context.Context, az *acme.Authorization) error {
	if strings.HasPrefix(az.Identifier.Value, "*.") {
		az.Wildcard = true
		az.Identifier = acme.Identifier{
			Value: strings.TrimPrefix(az.Identifier.Value, "*."),
			Type:  az.Identifier.Type,
		}
	}

	chTypes := challengeTypes(az)

	var err error
	az.Token, err = randutil.Alphanumeric(32)
	if err != nil {
		return acme.WrapErrorISE(err, "error generating random alphanumeric ID")
	}

	db := acme.MustDatabaseFromContext(ctx)
	az.Challenges = make([]*acme.Challenge, len(chTypes))
	for i, typ := range chTypes {
		ch := &acme.Challenge{
			AccountID: az.AccountID,
			Value:     az.Identifier.Value,
			Type:      typ,
			Token:     az.Token,
			Status:    acme.StatusPending,
		}
		if err := db.CreateChallenge(ctx, ch); err != nil {
			return acme.WrapErrorISE(err, "error creating challenge")
		}
		az.Challenges[i] = ch
	}
	if err = db.CreateAuthorization(ctx, az); err != nil {
		return acme.WrapErrorISE(err, "error creating authorization")
	}
	return nil
}

// GetOrder ACME api for retrieving an order.
func GetOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	db := acme.MustDatabaseFromContext(ctx)
	linker := acme.MustLinkerFromContext(ctx)

	acc, err := accountFromContext(ctx)
	if err != nil {
		render.Error(w, err)
		return
	}
	prov, err := provisionerFromContext(ctx)
	if err != nil {
		render.Error(w, err)
		return
	}

	o, err := db.GetOrder(ctx, chi.URLParam(r, "ordID"))
	if err != nil {
		render.Error(w, acme.WrapErrorISE(err, "error retrieving order"))
		return
	}
	if acc.ID != o.AccountID {
		render.Error(w, acme.NewError(acme.ErrorUnauthorizedType,
			"account '%s' does not own order '%s'", acc.ID, o.ID))
		return
	}
	if prov.GetID() != o.ProvisionerID {
		render.Error(w, acme.NewError(acme.ErrorUnauthorizedType,
			"provisioner '%s' does not own order '%s'", prov.GetID(), o.ID))
		return
	}
	if err = o.UpdateStatus(ctx, db); err != nil {
		render.Error(w, acme.WrapErrorISE(err, "error updating order status"))
		return
	}

	linker.LinkOrder(ctx, o)

	w.Header().Set("Location", linker.GetLink(ctx, acme.OrderLinkType, o.ID))
	render.JSON(w, o)
}

// FinalizeOrder attemptst to finalize an order and create a certificate.
func FinalizeOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	db := acme.MustDatabaseFromContext(ctx)
	linker := acme.MustLinkerFromContext(ctx)

	acc, err := accountFromContext(ctx)
	if err != nil {
		render.Error(w, err)
		return
	}
	prov, err := provisionerFromContext(ctx)
	if err != nil {
		render.Error(w, err)
		return
	}
	payload, err := payloadFromContext(ctx)
	if err != nil {
		render.Error(w, err)
		return
	}
	var fr FinalizeRequest
	if err := json.Unmarshal(payload.value, &fr); err != nil {
		render.Error(w, acme.WrapError(acme.ErrorMalformedType, err,
			"failed to unmarshal finalize-order request payload"))
		return
	}
	if err := fr.Validate(); err != nil {
		render.Error(w, err)
		return
	}

	o, err := db.GetOrder(ctx, chi.URLParam(r, "ordID"))
	if err != nil {
		render.Error(w, acme.WrapErrorISE(err, "error retrieving order"))
		return
	}
	if acc.ID != o.AccountID {
		render.Error(w, acme.NewError(acme.ErrorUnauthorizedType,
			"account '%s' does not own order '%s'", acc.ID, o.ID))
		return
	}
	if prov.GetID() != o.ProvisionerID {
		render.Error(w, acme.NewError(acme.ErrorUnauthorizedType,
			"provisioner '%s' does not own order '%s'", prov.GetID(), o.ID))
		return
	}

	ca := mustAuthority(ctx)
	if err = o.Finalize(ctx, db, fr.csr, ca, prov); err != nil {
		render.Error(w, acme.WrapErrorISE(err, "error finalizing order"))
		return
	}

	linker.LinkOrder(ctx, o)

	w.Header().Set("Location", linker.GetLink(ctx, acme.OrderLinkType, o.ID))
	render.JSON(w, o)
}

// challengeTypes determines the types of challenges that should be used
// for the ACME authorization request.
func challengeTypes(az *acme.Authorization) []acme.ChallengeType {
	var chTypes []acme.ChallengeType

	switch az.Identifier.Type {
	case acme.IP:
		chTypes = []acme.ChallengeType{acme.HTTP01, acme.TLSALPN01}
	case acme.DNS:
		chTypes = []acme.ChallengeType{acme.DNS01}
		// HTTP and TLS challenges can only be used for identifiers without wildcards.
		if !az.Wildcard {
			chTypes = append(chTypes, []acme.ChallengeType{acme.HTTP01, acme.TLSALPN01}...)
		}
	default:
		chTypes = []acme.ChallengeType{}
	}

	return chTypes
}
