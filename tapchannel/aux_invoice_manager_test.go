package tapchannel

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/fn"
	"github.com/lightninglabs/taproot-assets/rfq"
	"github.com/lightninglabs/taproot-assets/rfqmath"
	"github.com/lightninglabs/taproot-assets/rfqmsg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

const (
	// The test channel ID to use across the test cases.
	testChanID = 1234

	// maxRandomInvoiceValueMSat is the maximum invoice value in mSAT to be
	// generated by the property based tests.
	maxRandomInvoiceValueMSat = 100_000_000_000

	// minRouteHints is the minimum length of route hints to be included in
	// an invoice.
	minRouteHints = 0

	// maxRouteHints is the maximum length of route hints to be included in
	// an invoice.
	maxRouteHints = 4

	// minHopHints is the minimum length of hop hints to be included in a
	// route hint.
	minHopHints = 0

	// maxHopHints is the maximum length of hop hints to be included in a
	// route hint.
	maxHopHints = 4
)

var (
	// The node ID to be used for the RFQ peer.
	testNodeID = route.Vertex{1, 2, 3}

	assetRate = big.NewInt(100_000)

	testAssetRate = rfqmath.FixedPoint[rfqmath.BigInt]{
		Coefficient: rfqmath.NewBigInt(assetRate),
		Scale:       0,
	}
)

// mockRfqManager mocks the interface of the rfq manager required by the aux
// invoice manager. It also holds some internal state to return the desired
// quotes.
type mockRfqManager struct {
	peerBuyQuotes   rfq.BuyAcceptMap
	localSellQuotes rfq.SellAcceptMap
}

func (m *mockRfqManager) PeerAcceptedBuyQuotes() rfq.BuyAcceptMap {
	return m.peerBuyQuotes
}

func (m *mockRfqManager) LocalAcceptedSellQuotes() rfq.SellAcceptMap {
	return m.localSellQuotes
}

// mockHtlcModifier mocks the HtlcModifier interface that is required by the
// AuxInvoiceManager.
type mockHtlcModifier struct {
	requestQue     []lndclient.InvoiceHtlcModifyRequest
	expectedResQue []lndclient.InvoiceHtlcModifyResponse
	done           chan bool
	t              *testing.T
}

// HtlcModifier handles the invoice htlc modification requests que, then checks
// the returned error and response against the expected values.
func (m *mockHtlcModifier) HtlcModifier(ctx context.Context,
	handler lndclient.InvoiceHtlcModifyHandler) error {

	// Process the requests that are provided by the test case.
	for i, r := range m.requestQue {
		res, err := handler(ctx, r)

		if err != nil {
			return err
		}

		if m.expectedResQue[i].CancelSet {
			if !res.CancelSet {
				return fmt.Errorf("expected cancel set flag")
			}

			continue
		}

		// Check if there's a match with the expected outcome.
		if res.AmtPaid != m.expectedResQue[i].AmtPaid {
			return fmt.Errorf("invoice paid amount does not match "+
				"expected amount, %v != %v", res.AmtPaid,
				m.expectedResQue[i].AmtPaid)
		}
	}

	// Signal that the htlc modifications are completed.
	close(m.done)

	return nil
}

// mockHtlcModifierProperty mocks the HtlcModifier interface that is required
// by the AuxHtlcModifier. This mock is specific to the property based tests,
// as some more info are needed to run more in-depth checks.
type mockHtlcModifierProperty struct {
	requestQue []lndclient.InvoiceHtlcModifyRequest
	rfqMap     rfq.BuyAcceptMap
	done       chan bool
	t          *rapid.T
}

// RfqPeerFromScid retrieves the peer associated with the RFQ id that is mapped
// to the provided scid, if it exists.
func (m *mockHtlcModifierProperty) RfqPeerFromScid(
	scid uint64) (route.Vertex, error) {

	buyQuote, ok := m.rfqMap[rfqmsg.SerialisedScid(scid)]
	if !ok {
		return route.Vertex{},
			fmt.Errorf("no peer found for RFQ SCID %d", scid)
	}

	return buyQuote.Peer, nil
}

// HtlcModifier is the version of the HtlcModifier used by the property based
// tests. It handles a que of htlc modification requests, then depending on the
// request and the context it checks the results against the expected behavior.
func (m *mockHtlcModifierProperty) HtlcModifier(ctx context.Context,
	handler lndclient.InvoiceHtlcModifyHandler) error {

	// Process the requests that are provided by the test case.
	for _, r := range m.requestQue {
		res, err := handler(ctx, r)
		if err != nil {
			if r.Invoice == nil {
				if !assert.ErrorContains(
					m.t, err, "cannot handle empty invoice",
				) {

					m.t.Errorf("expected empty invoice err")
				}
			} else {
				if !assert.ErrorContains(
					m.t, err, "price from quote",
				) {

					m.t.Errorf("expected quote price err")
				}
			}

			continue
		}

		if len(r.WireCustomRecords) == 0 {
			if isAssetInvoice(r.Invoice, m) {
				if !res.CancelSet {
					m.t.Errorf("expected cancel set flag")
				}
				continue
			}

			if r.ExitHtlcAmt != res.AmtPaid {
				m.t.Errorf("AmtPaid != ExitHtlcAmt")
			}
		}

		htlcBlob, err := r.WireCustomRecords.Serialize()
		require.NoError(m.t, err)

		htlc, err := rfqmsg.DecodeHtlc(htlcBlob)
		require.NoError(m.t, err)

		if htlc.RfqID.ValOpt().IsNone() {
			if r.ExitHtlcAmt != res.AmtPaid ||
				r.CircuitKey != res.CircuitKey {

				m.t.Errorf("exit amt and circuit key mismatch")
			}

			continue
		}

		rfqID := htlc.RfqID.ValOpt().UnsafeFromSome()

		quote, ok := m.rfqMap[rfqID.Scid()]
		if !ok {
			m.t.Errorf("no rfq quote found")
		}

		assetRate := lnwire.MilliSatoshi(quote.AssetRate.ToUint64())
		msatPerBtc := float64(btcutil.SatoshiPerBitcoin * 1000)
		unitValue := msatPerBtc / float64(assetRate)
		assetUnits := lnwire.MilliSatoshi(htlc.Amounts.Val.Sum())

		floatValue := float64(assetUnits) * unitValue

		assetValueMsat := lnwire.MilliSatoshi(floatValue)

		acceptedMsat := lnwire.MilliSatoshi(0)
		for _, htlc := range r.Invoice.Htlcs {
			acceptedMsat += lnwire.MilliSatoshi(htlc.AmtMsat)
		}

		marginHtlcs := len(r.Invoice.Htlcs) + 1
		marginMsat := lnwire.MilliSatoshi(
			float64(marginHtlcs) * unitValue,
		)

		totalMsatIn := marginMsat + assetValueMsat + acceptedMsat + 1

		invoiceValue := lnwire.MilliSatoshi(r.Invoice.ValueMsat)

		if totalMsatIn >= invoiceValue {
			if (invoiceValue - acceptedMsat) != res.AmtPaid {
				m.t.Errorf("amt + accepted != invoice amt")
			}
		} else {
			if assetValueMsat != res.AmtPaid {
				m.t.Errorf("unexpected final asset value")
			}
		}
	}

	// Signal that the htlc modifications are completed.
	close(m.done)

	return nil
}

// TestAuxInvoiceManager tests that the htlc modifications of the aux invoice
// manager align with our expectations.
func TestAuxInvoiceManager(t *testing.T) {
	testCases := []struct {
		name            string
		buyQuotes       rfq.BuyAcceptMap
		sellQuotes      rfq.SellAcceptMap
		requests        []lndclient.InvoiceHtlcModifyRequest
		responses       []lndclient.InvoiceHtlcModifyResponse
		containedErrStr string
	}{
		{
			name: "non asset invoice",
			requests: []lndclient.InvoiceHtlcModifyRequest{
				{
					Invoice:     &lnrpc.Invoice{},
					ExitHtlcAmt: 1234,
				},
			},
			responses: []lndclient.InvoiceHtlcModifyResponse{
				{
					AmtPaid: 1234,
				},
			},
		},
		{
			name: "non asset routing hints",
			requests: []lndclient.InvoiceHtlcModifyRequest{
				{
					Invoice: &lnrpc.Invoice{
						RouteHints: testNonAssetHints(),
						ValueMsat:  1_000_000,
					},
					ExitHtlcAmt: 1234,
				},
			},
			responses: []lndclient.InvoiceHtlcModifyResponse{
				{
					AmtPaid: 1234,
				},
			},
			buyQuotes: map[rfq.SerialisedScid]rfqmsg.BuyAccept{
				testChanID: {
					Peer: testNodeID,
				},
			},
		},
		{
			name: "asset invoice, no custom records",
			requests: []lndclient.InvoiceHtlcModifyRequest{
				{
					Invoice: &lnrpc.Invoice{
						RouteHints:  testRouteHints(),
						PaymentAddr: []byte{1, 1, 1},
					},
					ExitHtlcAmt: 1234,
				},
			},
			responses: []lndclient.InvoiceHtlcModifyResponse{
				{
					CancelSet: true,
				},
			},
			buyQuotes: map[rfq.SerialisedScid]rfqmsg.BuyAccept{
				testChanID: {
					Peer: testNodeID,
				},
			},
		},
		{
			name: "asset invoice, custom records",
			requests: []lndclient.InvoiceHtlcModifyRequest{
				{
					Invoice: &lnrpc.Invoice{
						RouteHints:  testRouteHints(),
						ValueMsat:   3_000_000,
						PaymentAddr: []byte{1, 1, 1},
					},
					WireCustomRecords: newWireCustomRecords(
						t, []*rfqmsg.AssetBalance{
							rfqmsg.NewAssetBalance(
								dummyAssetID(1),
								3,
							),
						}, fn.Some(dummyRfqID(31)),
					),
				},
			},
			responses: []lndclient.InvoiceHtlcModifyResponse{
				{
					AmtPaid: 3_000_000,
				},
			},
			buyQuotes: rfq.BuyAcceptMap{
				fn.Ptr(dummyRfqID(31)).Scid(): {
					Peer:      testNodeID,
					AssetRate: testAssetRate,
				},
			},
		},
		{
			name: "asset invoice, not enough amt",
			requests: []lndclient.InvoiceHtlcModifyRequest{
				{
					Invoice: &lnrpc.Invoice{
						RouteHints:  testRouteHints(),
						ValueMsat:   10_000_000,
						PaymentAddr: []byte{1, 1, 1},
					},
					WireCustomRecords: newWireCustomRecords(
						t, []*rfqmsg.AssetBalance{
							rfqmsg.NewAssetBalance(
								dummyAssetID(1),
								4,
							),
						}, fn.Some(dummyRfqID(31)),
					),
					ExitHtlcAmt: 1234,
				},
			},
			responses: []lndclient.InvoiceHtlcModifyResponse{
				{
					AmtPaid: 4_000_000,
				},
			},
			buyQuotes: rfq.BuyAcceptMap{
				fn.Ptr(dummyRfqID(31)).Scid(): {
					Peer:      testNodeID,
					AssetRate: testAssetRate,
				},
			},
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Logf("Running AuxInvoiceManager test case: %v", testCase.name)

		// Instantiate mock rfq manager.
		mockRfq := &mockRfqManager{
			peerBuyQuotes:   testCase.buyQuotes,
			localSellQuotes: testCase.sellQuotes,
		}

		done := make(chan bool)

		// Instantiate mock htlc modifier.
		mockModifier := &mockHtlcModifier{
			requestQue:     testCase.requests,
			expectedResQue: testCase.responses,
			done:           done,
			t:              t,
		}

		// Create the manager.
		manager := NewAuxInvoiceManager(
			&InvoiceManagerConfig{
				ChainParams:         testChainParams,
				InvoiceHtlcModifier: mockModifier,
				RfqManager:          mockRfq,
			},
		)

		err := manager.Start()
		require.NoError(t, err)

		// If the manager is not done processing the htlc modification
		// requests within the specified timeout, assume this is a
		// failure.
		select {
		case <-done:
		case <-time.After(testTimeout):
			t.Fail()
		}
	}
}

// genRandomRfqID generates a random rfqmsg.ID value.
func genRandomRfqID(t *rapid.T) rfqmsg.ID {
	return rapid.Make[[32]byte]().Draw(t, "rfq_id")
}

// genInvoice generates an invoice that may have a random amount, and may have
// routing hints.
func genInvoice(t *rapid.T, rfqID rfqmsg.ID) *lnrpc.Invoice {
	// Introduce a chance of a null invoice.
	if !rapid.Bool().Draw(t, "inv_exists") {
		return nil
	}

	res := &lnrpc.Invoice{}

	// Generate a random invoice value.
	res.ValueMsat = rapid.Int64Range(
		1, maxRandomInvoiceValueMSat,
	).Draw(t, "invoice_value_msat")

	res.RouteHints = genRouteHints(t, rfqID)

	return res
}

// genRouteHints generates route hints for an invoice. Given an rfqID, it may
// contain a hop hint that references that rfqID.
func genRouteHints(t *rapid.T, rfqID rfqmsg.ID) []*lnrpc.RouteHint {
	res := make([]*lnrpc.RouteHint, 0)

	rhLen := rapid.IntRange(
		minRouteHints, maxRouteHints,
	).Draw(t, "route_hints_len")

	for range rhLen {
		hh := genHopHints(t, rfqID)
		res = append(res, &lnrpc.RouteHint{HopHints: hh})
	}

	return res
}

// genHopHints generated random hop hints to be included as part of a route
// hint. They may have incorrect details.
func genHopHints(t *rapid.T, rfqID rfqmsg.ID) []*lnrpc.HopHint {
	res := make([]*lnrpc.HopHint, 0)

	hhLen := rapid.IntRange(
		minHopHints, maxHopHints,
	).Draw(t, "hop_hints_len")

	for range hhLen {
		hop := &lnrpc.HopHint{}

		// Introduce a chance of a bad SCID in the hop hint.
		if rapid.Bool().Draw(t, "hop_hint_bad_scid") {
			hop.ChanId = 314
		} else {
			hop.ChanId = uint64(rfqID.Scid())
		}

		// Introduce a chance of a bad node ID in the hop hint.
		if rapid.Bool().Draw(t, "incorrect_peer") {
			hop.NodeId = "random"
		} else {
			hop.NodeId = testNodeID.String()
		}

		res = append(res, hop)
	}

	return res
}

// genCustomRecords generates custom records that have a random amount of random
// asset units, and may have an SCID as routing hint.
func genCustomRecords(t *rapid.T, amtMsat int64,
	rfqID rfqmsg.ID) (lnwire.CustomRecords, uint64) {

	// Introduce a chance of no wire custom records.
	if rapid.Bool().Draw(t, "no_wire_custom_records") {
		return nil, 0
	}

	// Pick a random number of asset units. The amount of units may be as
	// small as 1/100th of the invoice mSats, or as big as 1000x the amount
	// of the invoice mSats.
	assetUnits := rapid.Uint64Range(
		uint64(amtMsat/100)+1,
		uint64(amtMsat*1000)+1,
	).Draw(t, "asset_units")

	balance := []*rfqmsg.AssetBalance{
		rfqmsg.NewAssetBalance(
			dummyAssetID(rapid.Byte().Draw(t, "asset_id")),
			assetUnits,
		),
	}

	htlc := genHtlc(t, balance, rfqID)

	customRecords, err := lnwire.ParseCustomRecords(htlc.Bytes())
	require.NoError(t, err)

	return customRecords, assetUnits
}

// genHtlc generates an instance of rfqmsg.Htlc with the provided asset amounts
// and rfqID.
func genHtlc(t *rapid.T, balance []*rfqmsg.AssetBalance,
	rfqID rfqmsg.ID) *rfqmsg.Htlc {

	// Introduce a chance of no rfqID in this htlc.
	if rapid.Bool().Draw(t, "has_rfqid") {
		return rfqmsg.NewHtlc(balance, fn.None[rfqmsg.ID]())
	}

	// Introduce a chance of a mismatch in the expected and actual htlc
	// rfqID.
	if rapid.Bool().Draw(t, "rfqid_match") {
		return rfqmsg.NewHtlc(balance, fn.Some(dummyRfqID(
			rapid.IntRange(0, 255).Draw(t, "scid"),
		)))
	}

	return rfqmsg.NewHtlc(balance, fn.Some(rfqID))
}

// genRequest generates an InvoiceHtlcModifyRequest with random values. This
// method also returns the assetUnits and the rfqID used by the htlc.
func genRequest(t *rapid.T) (lndclient.InvoiceHtlcModifyRequest, uint64,
	rfqmsg.ID) {

	request := lndclient.InvoiceHtlcModifyRequest{}

	rfqID := genRandomRfqID(t)

	request.Invoice = genInvoice(t, rfqID)

	recordsAmt := int64(0)
	if request.Invoice != nil {
		recordsAmt = request.Invoice.ValueMsat
	}

	wireRecords, assetUnits := genCustomRecords(
		t, recordsAmt, rfqID,
	)
	request.WireCustomRecords = wireRecords
	request.ExitHtlcAmt = lnwire.MilliSatoshi(recordsAmt)

	return request, assetUnits, rfqID
}

// genRequests generates a random array of requests to be processed by the
// AuxInvoiceManager. It also returns the rfq map with the related rfq quotes.
func genRequests(t *rapid.T) ([]lndclient.InvoiceHtlcModifyRequest,
	rfq.BuyAcceptMap) {

	rfqMap := rfq.BuyAcceptMap{}

	numRequests := rapid.IntRange(1, 5).Draw(t, "requestsLen")
	requests := make([]lndclient.InvoiceHtlcModifyRequest, 0)

	for range numRequests {
		req, numAssets, scid := genRequest(t)
		requests = append(requests, req)

		quoteAmt := uint64(0)
		if req.Invoice != nil {
			quoteAmt = uint64(req.Invoice.ValueMsat)
		}

		genBuyQuotes(t, rfqMap, numAssets, quoteAmt, scid)
	}

	return requests, rfqMap
}

// genRandomVertex generates a route.Vertex instance filled with random bytes.
func genRandomVertex(t *rapid.T) route.Vertex {
	var vertex route.Vertex
	for i := 0; i < len(vertex); i++ {
		vertex[i] = rapid.Byte().Draw(t, "vertex_byte")
	}

	return vertex
}

// genBuyQuotes populates the provided map of rfq quotes with the desired values
// for a specific
func genBuyQuotes(t *rapid.T, rfqMap rfq.BuyAcceptMap, units, amtMsat uint64,
	scid rfqmsg.ID) {

	// If the passed asset units is set to 0 this means that no wire custom
	// records were set. To avoid a division by zero in the lines below we
	// just set it to 1, this data is not relevant anymore.
	if units == 0 {
		units = 1
	}

	var peer route.Vertex
	var assetRate *big.Int

	// Introduce a chance that the quote's peerID is not correct.
	if rapid.Bool().Draw(t, "nodeID_mismatch") {
		peer = genRandomVertex(t)
	} else {
		peer = testNodeID
	}

	rfqScid := scid

	// Introduce a chance that the quote's peerID is not correct.
	if rapid.Bool().Draw(t, "scid_mismatch") {
		rfqScid = rfqmsg.ID{2, 3, 4}
	}

	// Introduce a chance that the askPrice of this asset will result in a
	// random total asset value.
	if rapid.Bool().Draw(t, "no_asset_value_match") {
		msatPerBtc := int64(btcutil.SatoshiPerBitcoin * 1000)
		msatPerUnit := (float64(amtMsat) / float64(units)) + 1
		assetRateInt := int64(float64(msatPerBtc)/msatPerUnit) + 1

		// For a random asset unit price, draw a random value between
		// 1/50th and double the expected price.
		assetRate = big.NewInt(rapid.Int64Range(
			assetRateInt/2, assetRateInt*50,
		).Draw(t, "asset_msat_value"))
	} else {
		msatPerBtc := int64(btcutil.SatoshiPerBitcoin * 1000)
		msatPerUnit := float64(amtMsat) / float64(units)
		assetRate = big.NewInt(
			int64(float64(msatPerBtc) / msatPerUnit),
		)
	}

	rfqMap[rfqScid.Scid()] = rfqmsg.BuyAccept{
		Peer: peer,
		AssetRate: rfqmath.FixedPoint[rfqmath.BigInt]{
			Coefficient: rfqmath.NewBigInt(assetRate),
			Scale:       0,
		},
	}
}

// testInvoiceManager creates an array of requests to be processed by the
// AuxInvoiceManager. Uses the enhanced HtlcModifierMockProperty instance.
func testInvoiceManager(t *rapid.T) {
	requests, rfqMap := genRequests(t)

	mockRfq := &mockRfqManager{
		peerBuyQuotes: rfqMap,
	}

	done := make(chan bool)

	mockModifier := &mockHtlcModifierProperty{
		requestQue: requests,
		rfqMap:     rfqMap,
		done:       done,
		t:          t,
	}

	manager := NewAuxInvoiceManager(
		&InvoiceManagerConfig{
			ChainParams:         testChainParams,
			InvoiceHtlcModifier: mockModifier,
			RfqManager:          mockRfq,
		},
	)

	err := manager.Start()
	require.NoError(t, err)

	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fail()
	}
}

// TestAuxInvoiceManagerProperty runs property based tests on the
// AuxInvoiceManager.
func TestAuxInvoiceManagerProperty(t *testing.T) {
	t.Parallel()

	t.Run("invoice_manager", rapid.MakeCheck(testInvoiceManager))
}

func newHash(i []byte) []byte {
	h := sha256.New()
	_, _ = h.Write(i)

	return h.Sum(nil)
}

func dummyAssetID(i byte) asset.ID {
	return asset.ID(newHash([]byte{i}))
}

func dummyRfqID(value int) rfqmsg.ID {
	return rfqmsg.ID(newHash([]byte{byte(value)}))
}

func testRouteHints() []*lnrpc.RouteHint {
	return []*lnrpc.RouteHint{
		{
			HopHints: []*lnrpc.HopHint{
				{
					ChanId: 1111,
					NodeId: route.Vertex{1, 1, 1}.String(),
				},
				{
					ChanId: 1111,
					NodeId: route.Vertex{1, 1, 1}.String(),
				},
			},
		},
		{
			HopHints: []*lnrpc.HopHint{
				{
					ChanId: 1233,
					NodeId: route.Vertex{1, 1, 1}.String(),
				},
				{
					ChanId: 1234,
					NodeId: route.Vertex{1, 2, 3}.String(),
				},
			},
		},
	}
}

func testNonAssetHints() []*lnrpc.RouteHint {
	return []*lnrpc.RouteHint{
		{
			HopHints: []*lnrpc.HopHint{
				{
					ChanId: 1234,
					NodeId: route.Vertex{1, 1, 1}.String(),
				},
				{
					ChanId: 1234,
					NodeId: route.Vertex{1, 1, 1}.String(),
				},
			},
		},
		{
			HopHints: []*lnrpc.HopHint{
				{
					ChanId: 1234,
					NodeId: route.Vertex{2, 2, 2}.String(),
				},
			},
		},
	}
}

func newWireCustomRecords(t *testing.T, amounts []*rfqmsg.AssetBalance,
	rfqID fn.Option[rfqmsg.ID]) lnwire.CustomRecords {

	htlc := rfqmsg.NewHtlc(amounts, rfqID)

	customRecords, err := lnwire.ParseCustomRecords(htlc.Bytes())
	require.NoError(t, err)

	return customRecords
}
