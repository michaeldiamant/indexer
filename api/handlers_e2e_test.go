package api

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand-sdk/encoding/json"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"

	"github.com/algorand/indexer/api/generated/v2"
	"github.com/algorand/indexer/idb"
	"github.com/algorand/indexer/idb/postgres"
	pgtest "github.com/algorand/indexer/idb/postgres/testing"
	"github.com/algorand/indexer/util/test"
)

func setupIdb(t *testing.T, genesis bookkeeping.Genesis, genesisBlock bookkeeping.Block) (*postgres.IndexerDb /*db*/, func() /*shutdownFunc*/) {
	_, connStr, shutdownFunc := pgtest.SetupPostgres(t)

	db, _, err := postgres.OpenPostgres(connStr, idb.IndexerDbOptions{}, nil)
	require.NoError(t, err)

	newShutdownFunc := func() {
		db.Close()
		shutdownFunc()
	}

	err = db.LoadGenesis(genesis)
	require.NoError(t, err)

	err = db.AddBlock(&genesisBlock)
	require.NoError(t, err)

	return db, newShutdownFunc
}

func TestApplicationHandlers(t *testing.T) {
	db, shutdownFunc := setupIdb(t, test.MakeGenesis(), test.MakeGenesisBlock())
	defer shutdownFunc()

	///////////
	// Given // A block containing an app call txn with ExtraProgramPages, that the creator and another account have opted into
	///////////

	const expectedAppIdx = 1 // must be 1 since this is the first txn
	const extraPages = 2
	txn := transactions.SignedTxnWithAD{
		SignedTxn: transactions.SignedTxn{
			Txn: transactions.Transaction{
				Type: "appl",
				Header: transactions.Header{
					Sender:      test.AccountA,
					GenesisHash: test.GenesisHash,
				},
				ApplicationCallTxnFields: transactions.ApplicationCallTxnFields{
					ApprovalProgram:   []byte{0x02, 0x20, 0x01, 0x01, 0x22},
					ClearStateProgram: []byte{0x02, 0x20, 0x01, 0x01, 0x22},
					ExtraProgramPages: extraPages,
				},
			},
			Sig: test.Signature,
		},
		ApplyData: transactions.ApplyData{
			ApplicationID: expectedAppIdx,
		},
	}
	optInTxnA := test.MakeAppOptInTxn(expectedAppIdx, test.AccountA)
	optInTxnB := test.MakeAppOptInTxn(expectedAppIdx, test.AccountB)

	block, err := test.MakeBlockForTxns(test.MakeGenesisBlock().BlockHeader, &txn, &optInTxnA, &optInTxnB)
	require.NoError(t, err)

	err = db.AddBlock(&block)
	require.NoError(t, err, "failed to commit")

	//////////
	// When // We query the app
	//////////

	setupReq := func(path, paramName, paramValue string) (echo.Context, *ServerImplementation, *httptest.ResponseRecorder) {
		e := echo.New()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		c.SetPath(path)
		c.SetParamNames(paramName)
		c.SetParamValues(paramValue)
		api := &ServerImplementation{db: db, timeout: 30 * time.Second}
		return c, api, rec
	}

	c, api, rec := setupReq("/v2/applications/:appidx", "appidx", strconv.Itoa(expectedAppIdx))
	params := generated.LookupApplicationByIDParams{}
	err = api.LookupApplicationByID(c, expectedAppIdx, params)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code, fmt.Sprintf("unexpected return code, body: %s", rec.Body.String()))

	//////////
	// Then // The response has non-zero ExtraProgramPages and other app data
	//////////

	checkApp := func(t *testing.T, app *generated.Application) {
		require.NotNil(t, app)
		require.NotNil(t, app.Params.ExtraProgramPages)
		require.Equal(t, uint64(extraPages), *app.Params.ExtraProgramPages)
		require.Equal(t, app.Id, uint64(expectedAppIdx))
		require.NotNil(t, app.Params.Creator)
		require.Equal(t, *app.Params.Creator, test.AccountA.String())
		require.Equal(t, app.Params.ApprovalProgram, []byte{0x02, 0x20, 0x01, 0x01, 0x22})
		require.Equal(t, app.Params.ClearStateProgram, []byte{0x02, 0x20, 0x01, 0x01, 0x22})
	}

	var response generated.ApplicationResponse
	data := rec.Body.Bytes()
	err = json.Decode(data, &response)
	require.NoError(t, err)
	checkApp(t, response.Application)

	t.Run("created-applications", func(t *testing.T) {
		//////////
		// When // We look up the app by creator address
		//////////

		c, api, rec := setupReq("/v2/accounts/:accountid/created-applications", "accountid", test.AccountA.String())
		params := generated.LookupAccountCreatedApplicationsParams{}
		err = api.LookupAccountCreatedApplications(c, test.AccountA.String(), params)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, rec.Code, fmt.Sprintf("unexpected return code, body: %s", rec.Body.String()))

		//////////
		// Then // The response has non-zero ExtraProgramPages and other app data
		//////////

		var response generated.ApplicationsResponse
		data := rec.Body.Bytes()
		err = json.Decode(data, &response)
		require.NoError(t, err)
		require.Len(t, response.Applications, 1)
		checkApp(t, &response.Applications[0])
	})

	checkAppLocalState := func(t *testing.T, ls *generated.ApplicationLocalState) {
		require.NotNil(t, ls)
		require.NotNil(t, ls.Deleted)
		require.False(t, *ls.Deleted)
		require.Equal(t, ls.Id, uint64(expectedAppIdx))
	}

	for _, tc := range []struct{ name, addr string }{
		{"creator", test.AccountA.String()},
		{"opted-in-account", test.AccountB.String()},
	} {
		t.Run("app-local-state-"+tc.name, func(t *testing.T) {
			//////////
			// When // We look up the app's local state for an address that has opted in
			//////////

			c, api, rec := setupReq("/v2/accounts/:accountid/apps-local-state", "accountid", test.AccountA.String())
			params := generated.LookupAccountAppLocalStatesParams{}
			err = api.LookupAccountAppLocalStates(c, tc.addr, params)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, rec.Code, fmt.Sprintf("unexpected return code, body: %s", rec.Body.String()))

			//////////
			// Then // AppLocalState is available for that address
			//////////

			var response generated.ApplicationLocalStatesResponse
			data := rec.Body.Bytes()
			err = json.Decode(data, &response)
			require.NoError(t, err)
			require.Len(t, response.AppsLocalStates, 1)
			checkAppLocalState(t, &response.AppsLocalStates[0])
		})
	}
}

func TestAccountExcludeParameters(t *testing.T) {
	db, shutdownFunc := setupIdb(t, test.MakeGenesis(), test.MakeGenesisBlock())
	defer shutdownFunc()

	///////////
	// Given // A block containing a creator of an app, an asset, who also holds and has opted-into those apps.
	///////////

	const expectedAppIdx = 1 // must be 1 since this is the first txn
	const expectedAssetIdx = 2
	createAppTxn := test.MakeCreateAppTxn(test.AccountA)
	createAssetTxn := test.MakeAssetConfigTxn(0, 100, 0, false, "UNIT", "Asset 2", "http://asset2.com", test.AccountA)
	appOptInTxnA := test.MakeAppOptInTxn(expectedAppIdx, test.AccountA)
	appOptInTxnB := test.MakeAppOptInTxn(expectedAppIdx, test.AccountB)
	assetOptInTxnA := test.MakeAssetOptInTxn(expectedAssetIdx, test.AccountA)
	assetOptInTxnB := test.MakeAssetOptInTxn(expectedAssetIdx, test.AccountB)

	block, err := test.MakeBlockForTxns(test.MakeGenesisBlock().BlockHeader, &createAppTxn, &createAssetTxn,
		&appOptInTxnA, &appOptInTxnB, &assetOptInTxnA, &assetOptInTxnB)
	require.NoError(t, err)

	err = db.AddBlock(&block)
	require.NoError(t, err, "failed to commit")

	//////////
	// When // We look up the address using various exclude parameters.
	//////////

	setupReq := func(path, paramName, paramValue string) (echo.Context, *ServerImplementation, *httptest.ResponseRecorder) {
		e := echo.New()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		c.SetPath(path)
		c.SetParamNames(paramName)
		c.SetParamValues(paramValue)
		api := &ServerImplementation{db: db, timeout: 30 * time.Second}
		return c, api, rec
	}

	//////////
	// Then // Those parameters are excluded.
	//////////

	testCases := []struct {
		address   basics.Address
		exclude   []string
		check     func(*testing.T, generated.AccountResponse)
		errStatus int
	}{{
		address: test.AccountA,
		exclude: []string{"all"},
		check: func(t *testing.T, r generated.AccountResponse) {
			require.Nil(t, r.Account.CreatedAssets)
			require.Nil(t, r.Account.CreatedApps)
			require.Nil(t, r.Account.Assets)
			require.Nil(t, r.Account.AppsLocalState)
		}}, {
		address: test.AccountA,
		exclude: []string{"created-assets", "created-apps", "apps-local-state", "assets"},
		check: func(t *testing.T, r generated.AccountResponse) {
			require.Nil(t, r.Account.CreatedAssets)
			require.Nil(t, r.Account.CreatedApps)
			require.Nil(t, r.Account.Assets)
			require.Nil(t, r.Account.AppsLocalState)
		}}, {
		address: test.AccountA,
		exclude: []string{"created-assets"},
		check: func(t *testing.T, r generated.AccountResponse) {
			require.Nil(t, r.Account.CreatedAssets)
			require.NotNil(t, r.Account.CreatedApps)
			require.NotNil(t, r.Account.Assets)
			require.NotNil(t, r.Account.AppsLocalState)
		}}, {
		address: test.AccountA,
		exclude: []string{"created-apps"},
		check: func(t *testing.T, r generated.AccountResponse) {
			require.NotNil(t, r.Account.CreatedAssets)
			require.Nil(t, r.Account.CreatedApps)
			require.NotNil(t, r.Account.Assets)
			require.NotNil(t, r.Account.AppsLocalState)
		}}, {
		address: test.AccountA,
		exclude: []string{"apps-local-state"},
		check: func(t *testing.T, r generated.AccountResponse) {
			require.NotNil(t, r.Account.CreatedAssets)
			require.NotNil(t, r.Account.CreatedApps)
			require.NotNil(t, r.Account.Assets)
			require.Nil(t, r.Account.AppsLocalState)
		}}, {
		address: test.AccountA,
		exclude: []string{"assets"},
		check: func(t *testing.T, r generated.AccountResponse) {
			require.NotNil(t, r.Account.CreatedAssets)
			require.NotNil(t, r.Account.CreatedApps)
			require.Nil(t, r.Account.Assets)
			require.NotNil(t, r.Account.AppsLocalState)
		}}, {
		address: test.AccountB,
		exclude: []string{"assets", "apps-local-state"},
		check: func(t *testing.T, r generated.AccountResponse) {
			require.Nil(t, r.Account.CreatedAssets)
			require.Nil(t, r.Account.CreatedApps)
			require.Nil(t, r.Account.Assets)
			require.Nil(t, r.Account.AppsLocalState)
		}},
		{
			address:   test.AccountA,
			exclude:   []string{"abc"},
			errStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("exclude %v", tc.exclude), func(t *testing.T) {
			c, api, rec := setupReq("/v2/accounts/:account-id", "account-id", tc.address.String())
			err := api.LookupAccountByID(c, tc.address.String(), generated.LookupAccountByIDParams{Exclude: &tc.exclude})
			require.NoError(t, err)
			if tc.errStatus != 0 {
				require.Equal(t, tc.errStatus, rec.Code)
				return
			}
			require.Equal(t, http.StatusOK, rec.Code, fmt.Sprintf("unexpected return code, body: %s", rec.Body.String()))
			data := rec.Body.Bytes()
			var response generated.AccountResponse
			err = json.Decode(data, &response)
			require.NoError(t, err)
			tc.check(t, response)
		})
	}

}

func TestAccountMaxResultsLimit(t *testing.T) {
	db, shutdownFunc := setupIdb(t, test.MakeGenesis(), test.MakeGenesisBlock())
	defer shutdownFunc()

	///////////
	// Given // A block containing an address that has created 5 apps and 5 assets, and another address that has opted into them all
	///////////

	expectedAppIDs := []uint64{1, 2, 3, 4, 5}
	expectedAssetIDs := []uint64{6, 7, 8, 9, 10}

	var txns []transactions.SignedTxnWithAD
	for range expectedAppIDs {
		txns = append(txns, test.MakeCreateAppTxn(test.AccountA))
	}

	for i := range expectedAssetIDs {
		txns = append(txns, test.MakeAssetConfigTxn(0, 100, 0, false, "UNIT",
			fmt.Sprintf("Asset %d", expectedAssetIDs[i]), "http://asset.com", test.AccountA))
	}

	for _, id := range expectedAppIDs {
		txns = append(txns, test.MakeAppOptInTxn(id, test.AccountA))
		txns = append(txns, test.MakeAppOptInTxn(id, test.AccountB))
	}
	for _, id := range expectedAssetIDs {
		txns = append(txns, test.MakeAssetOptInTxn(id, test.AccountA))
		txns = append(txns, test.MakeAssetOptInTxn(id, test.AccountB))
	}

	ptxns := make([]*transactions.SignedTxnWithAD, len(txns))
	for i := range txns {
		ptxns[i] = &txns[i]
	}
	block, err := test.MakeBlockForTxns(test.MakeGenesisBlock().BlockHeader, ptxns...)
	require.NoError(t, err)

	err = db.AddBlock(&block)
	require.NoError(t, err, "failed to commit")

	//////////
	// When // We look up the address using a ServerImplementation with a maxAccountsAPIResults limit set,
	//      // and addresses with max # apps over & under the limit
	//////////

	setupReq := func(path, paramName, paramValue string, maxResults int) (echo.Context, *ServerImplementation, *httptest.ResponseRecorder) {
		e := echo.New()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		c.SetPath(path)
		c.SetParamNames(paramName)
		c.SetParamValues(paramValue)
		api := &ServerImplementation{db: db, timeout: 30 * time.Second, maxAccountsAPIResults: uint32(maxResults)}
		return c, api, rec
	}

	//////////
	// Then // The limit is enforced, leading to a 400 error
	//////////

	testCases := []struct {
		address   basics.Address
		exclude   []string
		check     func(*testing.T, generated.AccountResponse)
		errStatus int
	}{{
		address: test.AccountA,
		exclude: []string{"all"},
		check: func(t *testing.T, r generated.AccountResponse) {
			require.Nil(t, r.Account.CreatedAssets)
			require.Nil(t, r.Account.CreatedApps)
			require.Nil(t, r.Account.Assets)
			require.Nil(t, r.Account.AppsLocalState)
		}}, {
		address: test.AccountA,
		exclude: []string{"created-assets", "created-apps", "apps-local-state", "assets"},
		check: func(t *testing.T, r generated.AccountResponse) {
			require.Nil(t, r.Account.CreatedAssets)
			require.Nil(t, r.Account.CreatedApps)
			require.Nil(t, r.Account.Assets)
			require.Nil(t, r.Account.AppsLocalState)
		}}, {
		address: test.AccountA,
		exclude: []string{"created-assets"},
		check: func(t *testing.T, r generated.AccountResponse) {
			require.Nil(t, r.Account.CreatedAssets)
			require.NotNil(t, r.Account.CreatedApps)
			require.NotNil(t, r.Account.Assets)
			require.NotNil(t, r.Account.AppsLocalState)
		}}, {
		address: test.AccountA,
		exclude: []string{"created-apps"},
		check: func(t *testing.T, r generated.AccountResponse) {
			require.NotNil(t, r.Account.CreatedAssets)
			require.Nil(t, r.Account.CreatedApps)
			require.NotNil(t, r.Account.Assets)
			require.NotNil(t, r.Account.AppsLocalState)
		}}, {
		address: test.AccountA,
		exclude: []string{"apps-local-state"},
		check: func(t *testing.T, r generated.AccountResponse) {
			require.NotNil(t, r.Account.CreatedAssets)
			require.NotNil(t, r.Account.CreatedApps)
			require.NotNil(t, r.Account.Assets)
			require.Nil(t, r.Account.AppsLocalState)
		}}, {
		address: test.AccountA,
		exclude: []string{"assets"},
		check: func(t *testing.T, r generated.AccountResponse) {
			require.NotNil(t, r.Account.CreatedAssets)
			require.NotNil(t, r.Account.CreatedApps)
			require.Nil(t, r.Account.Assets)
			require.NotNil(t, r.Account.AppsLocalState)
		}}, {
		address: test.AccountB,
		exclude: []string{"assets", "apps-local-state"},
		check: func(t *testing.T, r generated.AccountResponse) {
			require.Nil(t, r.Account.CreatedAssets)
			require.Nil(t, r.Account.CreatedApps)
			require.Nil(t, r.Account.Assets)
			require.Nil(t, r.Account.AppsLocalState)
		}},
		{
			address:   test.AccountA,
			exclude:   []string{"abc"},
			errStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("exclude %v", tc.exclude), func(t *testing.T) {
			c, api, rec := setupReq("/v2/accounts/:account-id", "account-id", tc.address.String(), 100)
			err := api.LookupAccountByID(c, tc.address.String(), generated.LookupAccountByIDParams{Exclude: &tc.exclude})
			require.NoError(t, err)
			if tc.errStatus != 0 {
				require.Equal(t, tc.errStatus, rec.Code)
				return
			}
			require.Equal(t, http.StatusOK, rec.Code, fmt.Sprintf("unexpected return code, body: %s", rec.Body.String()))
			data := rec.Body.Bytes()
			var response generated.AccountResponse
			err = json.Decode(data, &response)
			require.NoError(t, err)
			tc.check(t, response)
		})
	}

}

func TestBlockNotFound(t *testing.T) {
	db, shutdownFunc := setupIdb(t, test.MakeGenesis(), test.MakeGenesisBlock())
	defer shutdownFunc()

	///////////
	// Given // An empty database.
	///////////

	//////////
	// When // We query for a non-existent block.
	//////////
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPath("/v2/blocks/:round-number")
	c.SetParamNames("round-number")
	c.SetParamValues(strconv.Itoa(100))

	api := &ServerImplementation{db: db, timeout: 30 * time.Second}
	err := api.LookupBlock(c, 100)
	require.NoError(t, err)

	//////////
	// Then // A 404 gets returned.
	//////////
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), errLookingUpBlockForRound)
}

// TestInnerTxn runs queries that return one or more root/inner transactions,
// and verifies that only a single root transaction is returned.
func TestInnerTxn(t *testing.T) {
	var appAddr basics.Address
	appAddr[1] = 99
	appAddrStr := appAddr.String()

	pay := "pay"
	axfer := "axfer"
	appl := "appl"
	testcases := []struct {
		name   string
		filter generated.SearchForTransactionsParams
	}{
		{
			name:   "match on root",
			filter: generated.SearchForTransactionsParams{Address: &appAddrStr, TxType: &pay},
		},
		{
			name:   "match on inner",
			filter: generated.SearchForTransactionsParams{Address: &appAddrStr, TxType: &pay},
		},
		{
			name:   "match on inner-inner",
			filter: generated.SearchForTransactionsParams{Address: &appAddrStr, TxType: &axfer},
		},
		{
			name:   "match on inner-inner",
			filter: generated.SearchForTransactionsParams{Address: &appAddrStr, TxType: &appl},
		},
		{
			name:   "match all",
			filter: generated.SearchForTransactionsParams{Address: &appAddrStr},
		},
	}

	db, shutdownFunc := setupIdb(t, test.MakeGenesis(), test.MakeGenesisBlock())
	defer shutdownFunc()

	///////////
	// Given // a DB with some inner txns in it.
	///////////
	appCall := test.MakeAppCallWithInnerTxn(test.AccountA, appAddr, test.AccountB, appAddr, test.AccountC)
	expectedID := appCall.Txn.ID().String()

	block, err := test.MakeBlockForTxns(test.MakeGenesisBlock().BlockHeader, &appCall)
	require.NoError(t, err)

	err = db.AddBlock(&block)
	require.NoError(t, err, "failed to commit")

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			//////////
			// When // we run a query that matches the Root Txn and/or Inner Txns
			//////////
			e := echo.New()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.SetPath("/v2/transactions/")

			api := &ServerImplementation{db: db, timeout: 30 * time.Second}
			err = api.SearchForTransactions(c, tc.filter)
			require.NoError(t, err)

			//////////
			// Then // The only result is the root transaction.
			//////////
			require.Equal(t, http.StatusOK, rec.Code)
			var response generated.TransactionsResponse
			json.Decode(rec.Body.Bytes(), &response)

			require.Len(t, response.Transactions, 1)
			require.Equal(t, expectedID, *(response.Transactions[0].Id))
		})
	}
}

// TestPagingRootTxnDeduplication checks that paging in the middle of an inner
// transaction group does not allow the root transaction to be returned on both
// pages.
func TestPagingRootTxnDeduplication(t *testing.T) {
	db, shutdownFunc := setupIdb(t, test.MakeGenesis(), test.MakeGenesisBlock())
	defer shutdownFunc()

	///////////
	// Given // a DB with some inner txns in it.
	///////////
	var appAddr basics.Address
	appAddr[1] = 99
	appAddrStr := appAddr.String()

	appCall := test.MakeAppCallWithInnerTxn(test.AccountA, appAddr, test.AccountB, appAddr, test.AccountC)
	expectedID := appCall.Txn.ID().String()

	block, err := test.MakeBlockForTxns(test.MakeGenesisBlock().BlockHeader, &appCall)
	require.NoError(t, err)

	err = db.AddBlock(&block)
	require.NoError(t, err, "failed to commit")

	testcases := []struct {
		name   string
		params generated.SearchForTransactionsParams
	}{
		{
			name:   "descending transaction search, middle of inner txns",
			params: generated.SearchForTransactionsParams{Address: &appAddrStr, Limit: uint64Ptr(1)},
		},
		{
			name:   "ascending transaction search, middle of inner txns",
			params: generated.SearchForTransactionsParams{Limit: uint64Ptr(2)},
		},
		{
			name:   "ascending transaction search, match root skip over inner txns",
			params: generated.SearchForTransactionsParams{Limit: uint64Ptr(1)},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			//////////
			// When // we match the first inner transaction and page to the next.
			//////////
			e := echo.New()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			rec1 := httptest.NewRecorder()
			c := e.NewContext(req, rec1)
			c.SetPath("/v2/transactions/")

			// Get first page with limit 1.
			// Address filter causes results to return newest to oldest.
			api := &ServerImplementation{db: db, timeout: 30 * time.Second}
			err = api.SearchForTransactions(c, tc.params)
			require.NoError(t, err)

			require.Equal(t, http.StatusOK, rec1.Code)
			var response generated.TransactionsResponse
			json.Decode(rec1.Body.Bytes(), &response)
			require.Len(t, response.Transactions, 1)
			require.Equal(t, expectedID, *(response.Transactions[0].Id))
			pageOneNextToken := *response.NextToken

			// Second page, using "NextToken" from first page.
			req = httptest.NewRequest(http.MethodGet, "/", nil)
			rec2 := httptest.NewRecorder()
			c = e.NewContext(req, rec2)
			c.SetPath("/v2/transactions/")

			// Set the next token
			tc.params.Next = &pageOneNextToken
			// In the debugger I see the internal call returning the inner tx + root tx
			err = api.SearchForTransactions(c, tc.params)
			require.NoError(t, err)

			//////////
			// Then // There are no new results on the next page.
			//////////
			var response2 generated.TransactionsResponse
			require.Equal(t, http.StatusOK, rec2.Code)
			json.Decode(rec2.Body.Bytes(), &response2)

			require.Len(t, response2.Transactions, 0)
			// The fact that NextToken changes indicates that the search results were different.
			if response2.NextToken != nil {
				require.NotEqual(t, pageOneNextToken, *response2.NextToken)
			}
		})
	}

	// Test block endpoint deduplication
	t.Run("Deduplicate Transactions In Block", func(t *testing.T) {
		//////////
		// When // we fetch the block
		//////////
		e := echo.New()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		c.SetPath("/v2/blocks/")

		// Get first page with limit 1.
		// Address filter causes results to return newest to oldest.
		api := &ServerImplementation{db: db, timeout: 30 * time.Second}
		err = api.LookupBlock(c, uint64(block.Round()))
		require.NoError(t, err)

		//////////
		// Then // There should be a single transaction which has inner transactions
		//////////
		var response generated.BlockResponse
		require.Equal(t, http.StatusOK, rec.Code)
		json.Decode(rec.Body.Bytes(), &response)

		require.NotNil(t, response.Transactions)
		require.Len(t, *response.Transactions, 1)
		require.NotNil(t, (*response.Transactions)[0])
		require.Len(t, *(*response.Transactions)[0].InnerTxns, 2)
	})
}

func TestVersion(t *testing.T) {
	///////////
	// Given // An API and context
	///////////
	db, shutdownFunc := setupIdb(t, test.MakeGenesis(), test.MakeGenesisBlock())
	defer shutdownFunc()
	api := &ServerImplementation{db: db, timeout: 30 * time.Second}

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec1 := httptest.NewRecorder()
	c := e.NewContext(req, rec1)

	//////////
	// When // we call the health endpoint
	//////////
	err := api.MakeHealthCheck(c)

	//////////
	// Then // We get the health information.
	//////////
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec1.Code)
	var response generated.HealthCheckResponse
	json.Decode(rec1.Body.Bytes(), &response)

	require.Equal(t, uint64(0), response.Round)
	require.False(t, response.IsMigrating)
	// This is weird looking because the version is set with -ldflags
	require.Equal(t, response.Version, "(unknown version)")
}

func TestAccountClearsNonUTF8(t *testing.T) {
	db, shutdownFunc := setupIdb(t, test.MakeGenesis(), test.MakeGenesisBlock())
	defer shutdownFunc()

	///////////
	// Given // a DB with some inner txns in it.
	///////////
	//var createAddr basics.Address
	//createAddr[1] = 99
	//createAddrStr := createAddr.String()

	assetName := "valid"
	//url := "https://my.embedded.\000.null.asset"
	urlBytes, _ := base64.StdEncoding.DecodeString("8J+qmSBNb25leQ==")
	url := string(urlBytes)
	unitName := "asset\rwith\nnon-printable\tcharacters"
	createAsset := test.MakeAssetConfigTxn(0, 100, 0, false, unitName, assetName, url, test.AccountA)

	block, err := test.MakeBlockForTxns(test.MakeGenesisBlock().BlockHeader, &createAsset)
	require.NoError(t, err)

	err = db.AddBlock(&block)
	require.NoError(t, err, "failed to commit")

	verify := func(params generated.AssetParams) {
		compareB64 := func(expected string, actual *[]byte) {
			actualStr := string(*actual)
			require.Equal(t, expected, actualStr)
		}

		// In all cases, the B64 encoded names should be the same.
		compareB64(assetName, params.NameB64)
		compareB64(unitName, params.UnitNameB64)
		compareB64(url, params.UrlB64)

		require.Equal(t, assetName, *params.Name, "valid utf8 should remain")
		require.Nil(t, params.UnitName, "null bytes should not be displayed")
		require.Nil(t, params.Url, "non printable characters should not be displayed")
	}

	{
		//////////
		// When // we lookup the asset
		//////////
		e := echo.New()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		c.SetPath("/v2/assets/")

		api := &ServerImplementation{db: db, timeout: 30 * time.Second}
		err = api.SearchForAssets(c, generated.SearchForAssetsParams{})
		require.NoError(t, err)

		require.Equal(t, http.StatusOK, rec.Code)
		var response generated.AssetsResponse
		json.Decode(rec.Body.Bytes(), &response)

		//////////
		// Then // we should find one asset with the expected string encodings
		//////////
		require.Len(t, response.Assets, 1)
		verify(response.Assets[0].Params)
	}

	{
		//////////
		// When // we lookup the account
		//////////
		e := echo.New()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		c.SetPath("/v2/accounts/")

		api := &ServerImplementation{db: db, timeout: 30 * time.Second}
		err = api.LookupAccountByID(c, test.AccountA.String(), generated.LookupAccountByIDParams{})
		require.NoError(t, err)

		require.Equal(t, http.StatusOK, rec.Code)
		var response generated.AccountResponse
		json.Decode(rec.Body.Bytes(), &response)

		//////////
		// Then // we should find one asset with the expected string encodings
		//////////
		require.NotNil(t, response.Account.CreatedAssets, 1)
		require.Len(t, *response.Account.CreatedAssets, 1)
		verify((*response.Account.CreatedAssets)[0].Params)
	}
}

// TestLookupInnerLogs runs queries for logs given application ids,
// and checks that logs in inner transactions match properly.
func TestLookupInnerLogs(t *testing.T) {
	var appAddr basics.Address
	appAddr[1] = 99

	params := generated.LookupApplicationLogsByIDParams{}

	testcases := []struct {
		name  string
		appID uint64
		logs  []string
	}{
		{
			name:  "match on root",
			appID: 123,
			logs: []string{
				"testing outer appl log",
				"appId 123 log",
			},
		},
		{
			name:  "match on inner",
			appID: 789,
			logs: []string{
				"testing inner log",
				"appId 789 log",
			},
		},
		{
			name:  "match on inner-inner",
			appID: 999,
			logs: []string{
				"testing inner-inner log",
				"appId 999 log",
			},
		},
	}

	db, shutdownFunc := setupIdb(t, test.MakeGenesis(), test.MakeGenesisBlock())
	defer shutdownFunc()

	///////////
	// Given // a DB with some inner txns in it.
	///////////
	appCall := test.MakeAppCallWithInnerAppCall(test.AccountA)

	block, err := test.MakeBlockForTxns(test.MakeGenesisBlock().BlockHeader, &appCall)
	require.NoError(t, err)

	err = db.AddBlock(&block)
	require.NoError(t, err, "failed to commit")

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			//////////
			// When // we run a query that queries logs based on appID
			//////////
			e := echo.New()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.SetPath("/v2/applications/:appIdx/logs")
			c.SetParamNames("appIdx")
			c.SetParamValues(fmt.Sprintf("%d", tc.appID))

			api := &ServerImplementation{db: db, timeout: 30 * time.Second}
			err = api.LookupApplicationLogsByID(c, tc.appID, params)
			require.NoError(t, err)

			//////////
			// Then // The result is the log from the app
			//////////
			var response generated.ApplicationLogsResponse
			require.Equal(t, http.StatusOK, rec.Code)
			json.Decode(rec.Body.Bytes(), &response)
			require.NoError(t, err)

			require.Equal(t, uint64(tc.appID), response.ApplicationId)
			require.NotNil(t, response.LogData)
			ld := *response.LogData
			require.Equal(t, 1, len(ld))
			require.Equal(t, len(tc.logs), len(ld[0].Logs))
			for i, log := range ld[0].Logs {
				require.Equal(t, []byte(tc.logs[i]), log)
			}
		})
	}
}
