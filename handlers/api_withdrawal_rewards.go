package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/gobitfly/eth2-beaconchain-explorer/db"
	"github.com/gobitfly/eth2-beaconchain-explorer/services"
	"github.com/gobitfly/eth2-beaconchain-explorer/utils"
	"github.com/gorilla/mux"
)

// WithdrawalRewardResponse represents rewards data linked to a withdrawal address
type WithdrawalRewardResponse struct {
	WithdrawalAddress string                `json:"withdrawal_address"`
	ValidatorCount    int64                 `json:"validator_count"`
	TotalRewards      string                `json:"total_rewards"`
	Validators        []ValidatorRewardInfo `json:"validators"`
}

// ValidatorRewardInfo contains validator details and its rewards
type ValidatorRewardInfo struct {
	ValidatorIndex    int64  `json:"validator_index"`
	ValidatorPubkey   string `json:"validator_pubkey"`
	WithdrawalAddress string `json:"withdrawal_address"`
	Balance           int64  `json:"balance"`
	EffectiveBalance  int64  `json:"effective_balance"`
	Status            string `json:"status"`
}

// ApiValidatorRewardsByWithdrawalAddress godoc
// @Summary Get all validators and their rewards for a specific withdrawal address
// @Tags Validator
// @Description Returns all validators associated with a withdrawal address along with their current rewards
// @Produce json
// @Param withdrawalAddress path string true "Withdrawal address (0x prefixed eth1 address)"
// @Success 200 {object} types.ApiResponse{data=WithdrawalRewardResponse}
// @Failure 400 {object} types.ApiResponse
// @Failure 500 {object} types.ApiResponse
// @Router /api/v1/validator/rewards/withdrawal/{withdrawalAddress} [get]
func ApiValidatorRewardsByWithdrawalAddress(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	vars := mux.Vars(r)
	withdrawalAddressStr := ReplaceEnsNameWithAddress(vars["withdrawalAddress"])
	withdrawalAddressStr = strings.ToLower(withdrawalAddressStr)

	// Validate withdrawal address format
	if !utils.IsValidEth1Address(withdrawalAddressStr) {
		SendBadRequestResponse(w, r.URL.String(), "invalid withdrawal address provided")
		return
	}

	// Convert to withdrawal credentials format
	addressBytes := common.FromHex(withdrawalAddressStr)
	withdrawalCredentials, err := utils.AddressToWithdrawalCredentials(addressBytes)
	if err != nil {
		SendBadRequestResponse(w, r.URL.String(), "could not process withdrawal address")
		return
	}

	// Get all validators with this withdrawal credential
	var validators []struct {
		Index            int64  `db:"validatorindex"`
		Pubkey           []byte `db:"pubkey"`
		Status           string `db:"status"`
		WithdrawalCreds  []byte `db:"withdrawalcredentials"`
	}

	err = db.ReaderDb.Select(&validators, `
		SELECT 
			validatorindex,
			pubkey,
			status,
			withdrawalcredentials
		FROM validators
		WHERE withdrawalcredentials = $1
		ORDER BY validatorindex ASC
	`, withdrawalCredentials)

	if err != nil {
		logger.Errorf("error retrieving validators by withdrawal address: %v", err)
		SendBadRequestResponse(w, r.URL.String(), "could not retrieve validators")
		return
	}

	if len(validators) == 0 {
		SendBadRequestResponse(w, r.URL.String(), "no validators found for this withdrawal address")
		return
	}

	// Extract validator indices
	validatorIndices := make([]uint64, len(validators))
	for i, v := range validators {
		validatorIndices[i] = uint64(v.Index)
	}

	// Get latest balances
	balances, err := db.BigtableClient.GetValidatorBalanceHistory(validatorIndices, services.LatestEpoch(), services.LatestEpoch())
	if err != nil {
		logger.Errorf("error retrieving validator balances: %v", err)
		SendBadRequestResponse(w, r.URL.String(), "could not retrieve balance data")
		return
	}

	// Build response
	response := WithdrawalRewardResponse{
		WithdrawalAddress: withdrawalAddressStr,
		ValidatorCount:    int64(len(validators)),
		Validators:        make([]ValidatorRewardInfo, 0),
	}

	totalRewardsWei := int64(0)

	for _, v := range validators {
		validatorInfo := ValidatorRewardInfo{
			ValidatorIndex:    v.Index,
			ValidatorPubkey:   fmt.Sprintf("0x%x", v.Pubkey),
			WithdrawalAddress: withdrawalAddressStr,
			Status:            v.Status,
		}

		// Get balance if available
		if balanceList, exists := balances[uint64(v.Index)]; exists && len(balanceList) > 0 {
			validatorInfo.Balance = int64(balanceList[0].Balance)
			validatorInfo.EffectiveBalance = int64(balanceList[0].EffectiveBalance)
			totalRewardsWei += int64(balanceList[0].Balance)
		}

		response.Validators = append(response.Validators, validatorInfo)
	}

	response.TotalRewards = fmt.Sprintf("%d", totalRewardsWei)

	j := json.NewEncoder(w)
	SendOKResponse(j, r.URL.String(), []interface{}{response})
}

// ApiWithdrawalAddressStats godoc
// @Summary Get aggregated statistics for all validators under a withdrawal address
// @Tags Validator
// @Description Returns combined stats for all validators sharing a withdrawal address
// @Produce json
// @Param withdrawalAddress path string true "Withdrawal address (0x prefixed eth1 address)"
// @Success 200 {object} types.ApiResponse
// @Failure 400 {object} types.ApiResponse
// @Router /api/v1/validator/stats/withdrawal/{withdrawalAddress} [get]
func ApiWithdrawalAddressStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	vars := mux.Vars(r)
	withdrawalAddressStr := ReplaceEnsNameWithAddress(vars["withdrawalAddress"])
	withdrawalAddressStr = strings.ToLower(withdrawalAddressStr)

	// Validate withdrawal address format
	if !utils.IsValidEth1Address(withdrawalAddressStr) {
		SendBadRequestResponse(w, r.URL.String(), "invalid withdrawal address provided")
		return
	}

	// Convert to withdrawal credentials format
	addressBytes := common.FromHex(withdrawalAddressStr)
	withdrawalCredentials, err := utils.AddressToWithdrawalCredentials(addressBytes)
	if err != nil {
		SendBadRequestResponse(w, r.URL.String(), "could not process withdrawal address")
		return
	}

	// Get stats using SQL query
	rows, err := db.ReaderDb.Query(`
		SELECT 
			COUNT(*) as validator_count,
			COUNT(CASE WHEN status = 'active' THEN 1 END) as active_count,
			COUNT(CASE WHEN status = 'exited' THEN 1 END) as exited_count,
			COUNT(CASE WHEN slashed = true THEN 1 END) as slashed_count,
			SUM(COALESCE(vp.cl_performance_7d, 0)) as performance_7d,
			MAX(COALESCE(vp.rank7d, 0)) as best_rank
		FROM validators v
		LEFT JOIN validator_performance vp ON v.validatorindex = vp.validatorindex
		WHERE v.withdrawalcredentials = $1
	`, withdrawalCredentials)

	if err != nil {
		logger.Errorf("error querying withdrawal address stats: %v", err)
		SendBadRequestResponse(w, r.URL.String(), "could not retrieve stats")
		return
	}
	defer rows.Close()

	returnQueryResults(rows, w, r)
}
