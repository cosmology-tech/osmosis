package twap_test

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"

	"github.com/osmosis-labs/osmosis/v13/app/apptesting/osmoassert"
	"github.com/osmosis-labs/osmosis/v13/osmomath"
	"github.com/osmosis-labs/osmosis/v13/osmoutils"
	gammtypes "github.com/osmosis-labs/osmosis/v13/x/gamm/types"
	"github.com/osmosis-labs/osmosis/v13/x/twap"
	"github.com/osmosis-labs/osmosis/v13/x/twap/types"
	"github.com/osmosis-labs/osmosis/v13/x/twap/types/twapmock"
)

var (
	zeroDec              = sdk.ZeroDec()
	oneDec               = sdk.OneDec()
	twoDec               = oneDec.Add(oneDec)
	pointFiveDec         = sdk.OneDec().Quo(twoDec)
	OneSec               = sdk.MustNewDecFromStr("1000.000000000000000000")
	logTen               = twap.TwapLog(sdk.NewDec(10))
	logOneOverTen        = twap.TwapLog(sdk.OneDec().QuoInt64(10))
	tenSecAccum          = OneSec.MulInt64(10)
	geometricTenSecAccum = OneSec.Mul(logTen)
)

func (s *TestSuite) TestGetSpotPrices() {
	currTime := time.Now()
	poolID := s.PrepareBalancerPoolWithCoins(defaultTwoAssetCoins...)
	mockAMMI := twapmock.NewProgrammedAmmInterface(s.App.TwapKeeper.GetAmmInterface())
	s.App.TwapKeeper.SetAmmInterface(mockAMMI)

	ctx := s.Ctx.WithBlockTime(currTime.Add(5 * time.Second))

	testCases := map[string]struct {
		poolID                uint64
		prevErrTime           time.Time
		mockSp0               sdk.Dec
		mockSp1               sdk.Dec
		mockSp0Err            error
		mockSp1Err            error
		expectedSp0           sdk.Dec
		expectedSp1           sdk.Dec
		expectedLatestErrTime time.Time
	}{
		"zero sp": {
			poolID:                poolID,
			prevErrTime:           currTime,
			mockSp0:               sdk.ZeroDec(),
			mockSp1:               sdk.ZeroDec(),
			mockSp0Err:            fmt.Errorf("foo"),
			expectedSp0:           sdk.ZeroDec(),
			expectedSp1:           sdk.ZeroDec(),
			expectedLatestErrTime: ctx.BlockTime(),
		},
		"exceeds max spot price": {
			poolID:                poolID,
			prevErrTime:           currTime,
			mockSp0:               types.MaxSpotPrice.Add(sdk.OneDec()),
			mockSp1:               types.MaxSpotPrice.Add(sdk.OneDec()),
			expectedSp0:           types.MaxSpotPrice,
			expectedSp1:           types.MaxSpotPrice,
			expectedLatestErrTime: ctx.BlockTime(),
		},
		"valid spot prices": {
			poolID:                poolID,
			prevErrTime:           currTime,
			mockSp0:               sdk.NewDecWithPrec(55, 2),
			mockSp1:               sdk.NewDecWithPrec(6, 1),
			expectedSp0:           sdk.NewDecWithPrec(55, 2),
			expectedSp1:           sdk.NewDecWithPrec(6, 1),
			expectedLatestErrTime: currTime,
		},
	}

	for name, tc := range testCases {
		s.Run(name, func() {
			mockAMMI.ProgramPoolSpotPriceOverride(tc.poolID, denom0, denom1, tc.mockSp0, tc.mockSp0Err)
			mockAMMI.ProgramPoolSpotPriceOverride(tc.poolID, denom1, denom0, tc.mockSp1, tc.mockSp1Err)

			sp0, sp1, latestErrTime := twap.GetSpotPrices(ctx, mockAMMI, tc.poolID, denom0, denom1, tc.prevErrTime)
			s.Require().Equal(tc.expectedSp0, sp0)
			s.Require().Equal(tc.expectedSp1, sp1)
			s.Require().Equal(tc.expectedLatestErrTime, latestErrTime)
		})
	}
}

func (s *TestSuite) TestNewTwapRecord() {
	// prepare pool before test
	poolId := s.PrepareBalancerPoolWithCoins(defaultTwoAssetCoins...)

	tests := map[string]struct {
		poolId        uint64
		denom0        string
		denom1        string
		expectedErr   error
		expectedPanic bool
	}{
		"denom with lexicographical order": {
			poolId,
			denom0,
			denom1,
			nil,
			false,
		},
		"denom with non-lexicographical order": {
			poolId,
			denom1,
			denom0,
			nil,
			false,
		},
		"new record with same denom": {
			poolId,
			denom0,
			denom0,
			fmt.Errorf("both assets cannot be of the same denom: assetA: %s, assetB: %s", denom0, denom0),
			false,
		},
		"error in getting spot price": {
			poolId + 1,
			denom1,
			denom0,
			nil,
			true,
		},
	}
	for name, test := range tests {
		s.Run(name, func() {
			twapRecord, err := twap.NewTwapRecord(s.App.GAMMKeeper, s.Ctx, test.poolId, test.denom0, test.denom1)

			if test.expectedPanic {
				s.Require().Equal(twapRecord.LastErrorTime, s.Ctx.BlockTime())
			} else if test.expectedErr != nil {
				s.Require().Error(err)
				s.Require().Equal(test.expectedErr.Error(), err.Error())
			} else {
				s.Require().NoError(err)

				s.Require().Equal(test.poolId, twapRecord.PoolId)
				s.Require().Equal(s.Ctx.BlockHeight(), twapRecord.Height)
				s.Require().Equal(s.Ctx.BlockTime(), twapRecord.Time)
				s.Require().Equal(sdk.ZeroDec(), twapRecord.P0ArithmeticTwapAccumulator)
				s.Require().Equal(sdk.ZeroDec(), twapRecord.P1ArithmeticTwapAccumulator)
				s.Require().Equal(sdk.ZeroDec(), twapRecord.GeometricTwapAccumulator)
			}
		})
	}
}

func (s *TestSuite) TestUpdateRecord() {
	poolId := s.PrepareBalancerPoolWithCoins(defaultTwoAssetCoins...)
	programmableAmmInterface := twapmock.NewProgrammedAmmInterface(s.App.TwapKeeper.GetAmmInterface())
	s.App.TwapKeeper.SetAmmInterface(programmableAmmInterface)

	spotPriceResOne := twapmock.SpotPriceResult{Sp: sdk.OneDec(), Err: nil}
	spotPriceResOneErr := twapmock.SpotPriceResult{Sp: sdk.OneDec(), Err: errors.New("dummy err")}
	spotPriceResOneErrNilDec := twapmock.SpotPriceResult{Sp: sdk.Dec{}, Err: errors.New("dummy err")}
	baseTime := time.Unix(2, 0).UTC()
	updateTime := time.Unix(3, 0).UTC()
	baseTimeMinusOne := time.Unix(1, 0).UTC()

	zeroAccumNoErrSp10Record := newRecord(poolId, baseTime, sdk.NewDec(10), zeroDec, zeroDec, zeroDec)
	sp10OneTimeUnitAccumRecord := newExpRecord(OneSec.MulInt64(10), OneSec.QuoInt64(10), geometricTenSecAccum)
	// all tests occur with updateTime = base time + time.Unix(1, 0)
	tests := map[string]struct {
		record           types.TwapRecord
		spotPriceResult0 twapmock.SpotPriceResult
		spotPriceResult1 twapmock.SpotPriceResult
		expRecord        types.TwapRecord
	}{
		"0 accum start, sp change": {
			record:           zeroAccumNoErrSp10Record,
			spotPriceResult0: spotPriceResOne,
			spotPriceResult1: spotPriceResOne,
			expRecord:        sp10OneTimeUnitAccumRecord,
		},
		"0 accum start, sp0 err at update": {
			record:           zeroAccumNoErrSp10Record,
			spotPriceResult0: spotPriceResOneErr,
			spotPriceResult1: spotPriceResOne,
			expRecord:        withLastErrTime(sp10OneTimeUnitAccumRecord, updateTime),
		},
		"0 accum start, sp0 err at update with nil dec": {
			record:           zeroAccumNoErrSp10Record,
			spotPriceResult0: spotPriceResOneErrNilDec,
			spotPriceResult1: spotPriceResOne,
			expRecord:        withSp0(withLastErrTime(sp10OneTimeUnitAccumRecord, updateTime), sdk.ZeroDec()),
		},
		"0 accum start, sp1 err at update with nil dec": {
			record:           zeroAccumNoErrSp10Record,
			spotPriceResult0: spotPriceResOne,
			spotPriceResult1: spotPriceResOneErrNilDec,
			expRecord:        withSp1(withLastErrTime(sp10OneTimeUnitAccumRecord, updateTime), sdk.ZeroDec()),
		},
		"startRecord err time preserved": {
			record:           withLastErrTime(zeroAccumNoErrSp10Record, baseTimeMinusOne),
			spotPriceResult0: spotPriceResOne,
			spotPriceResult1: spotPriceResOne,
			expRecord:        withLastErrTime(sp10OneTimeUnitAccumRecord, baseTimeMinusOne),
		},
		"err time bumped with start": {
			record:           withLastErrTime(zeroAccumNoErrSp10Record, baseTimeMinusOne),
			spotPriceResult0: spotPriceResOne,
			spotPriceResult1: spotPriceResOneErr,
			expRecord:        withLastErrTime(sp10OneTimeUnitAccumRecord, updateTime),
		},
	}
	for name, test := range tests {
		s.Run(name, func() {
			// setup common, block time, pool Id, expected spot prices
			s.Ctx = s.Ctx.WithBlockTime(updateTime.UTC())
			test.record.PoolId = poolId
			test.expRecord.PoolId = poolId
			if (test.expRecord.P0LastSpotPrice == sdk.Dec{}) {
				test.expRecord.P0LastSpotPrice = test.spotPriceResult0.Sp
			}
			if (test.expRecord.P1LastSpotPrice == sdk.Dec{}) {
				test.expRecord.P1LastSpotPrice = test.spotPriceResult1.Sp
			}
			test.expRecord.Height = s.Ctx.BlockHeight()
			test.expRecord.Time = s.Ctx.BlockTime()

			programmableAmmInterface.ProgramPoolSpotPriceOverride(poolId,
				defaultTwoAssetCoins[0].Denom, defaultTwoAssetCoins[1].Denom,
				test.spotPriceResult0.Sp, test.spotPriceResult0.Err)
			programmableAmmInterface.ProgramPoolSpotPriceOverride(poolId,
				defaultTwoAssetCoins[1].Denom, defaultTwoAssetCoins[0].Denom,
				test.spotPriceResult1.Sp, test.spotPriceResult1.Err)

			newRecord := s.twapkeeper.UpdateRecord(s.Ctx, test.record)
			s.Equal(test.expRecord, newRecord)
		})
	}
}

func TestRecordWithUpdatedAccumulators(t *testing.T) {
	poolId := uint64(1)
	defaultRecord := newRecord(poolId, time.Unix(1, 0), sdk.NewDec(10), oneDec, twoDec, pointFiveDec)
	tests := map[string]struct {
		record      types.TwapRecord
		newTime     time.Time
		expRecord   types.TwapRecord
		expectPanic bool
	}{
		"accum with zero value": {
			record:    newRecord(poolId, time.Unix(1, 0), sdk.NewDec(10), zeroDec, zeroDec, zeroDec),
			newTime:   time.Unix(2, 0),
			expRecord: newExpRecord(OneSec.MulInt64(10), OneSec.QuoInt64(10), geometricTenSecAccum),
		},
		"small starting accumulators": {
			record:    defaultRecord,
			newTime:   time.Unix(2, 0),
			expRecord: newExpRecord(oneDec.Add(OneSec.MulInt64(10)), twoDec.Add(OneSec.QuoInt64(10)), pointFiveDec.Add(geometricTenSecAccum)),
		},
		"larger time interval": {
			record:    newRecord(poolId, time.Unix(11, 0), sdk.NewDec(10), oneDec, twoDec, pointFiveDec),
			newTime:   time.Unix(55, 0),
			expRecord: newExpRecord(oneDec.Add(OneSec.MulInt64(44*10)), twoDec.Add(OneSec.MulInt64(44).QuoInt64(10)), pointFiveDec.Add(OneSec.MulInt64(44).Mul(logTen))),
		},
		"same time, accumulator should not change": {
			record:    defaultRecord,
			newTime:   time.Unix(1, 0),
			expRecord: newExpRecord(oneDec, twoDec, pointFiveDec),
		},
		"zero spot price - panic": {
			record:      withPrice0Set(defaultRecord, sdk.ZeroDec()),
			newTime:     defaultRecord.Time.Add(time.Second),
			expectPanic: true,
		},
		"spot price of one - geom accumulator 0": {
			record:    withPrice1Set(withPrice0Set(defaultRecord, sdk.OneDec()), sdk.OneDec()),
			newTime:   defaultRecord.Time.Add(time.Second),
			expRecord: newExpRecord(oneDec.Add(OneSec), twoDec.Add(OneSec), pointFiveDec),
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			// correct expected record based off copy/paste values
			test.expRecord.Time = test.newTime
			test.expRecord.PoolId = test.record.PoolId
			test.expRecord.P0LastSpotPrice = test.record.P0LastSpotPrice
			test.expRecord.P1LastSpotPrice = test.record.P1LastSpotPrice

			osmoassert.ConditionalPanic(t, test.expectPanic, func() {
				gotRecord := twap.RecordWithUpdatedAccumulators(test.record, test.newTime)
				require.Equal(t, test.expRecord, gotRecord)
			})
		})
	}
}

func TestRecordWithUpdatedAccumulators_ThreeAsset(t *testing.T) {
	poolId := uint64(2)
	tests := map[string]struct {
		record          []types.TwapRecord
		interpolateTime time.Time
		expRecord       []types.TwapRecord
	}{
		"accum with zero value": {
			record:          newThreeAssetRecord(poolId, time.Unix(1, 0), sdk.NewDec(10), zeroDec, zeroDec, zeroDec, zeroDec, zeroDec, zeroDec),
			interpolateTime: time.Unix(2, 0),
			expRecord:       newThreeAssetExpRecord(poolId, OneSec.MulInt64(10), OneSec.QuoInt64(10), OneSec.MulInt64(20), geometricTenSecAccum, geometricTenSecAccum, OneSec.Mul(logOneOverTen)),
		},
		"small starting accumulators": {
			record:          newThreeAssetRecord(poolId, time.Unix(1, 0), sdk.NewDec(10), twoDec, oneDec, twoDec, oneDec, twoDec, oneDec),
			interpolateTime: time.Unix(2, 0),
			expRecord:       newThreeAssetExpRecord(poolId, twoDec.Add(OneSec.MulInt64(10)), oneDec.Add(OneSec.QuoInt64(10)), twoDec.Add(OneSec.MulInt64(20)), oneDec.Add(geometricTenSecAccum), twoDec.Add(geometricTenSecAccum), oneDec.Add(OneSec.Mul(logOneOverTen))),
		},
		"larger time interval": {
			record:          newThreeAssetRecord(poolId, time.Unix(11, 0), sdk.NewDec(10), twoDec, oneDec, twoDec, oneDec, twoDec, oneDec),
			interpolateTime: time.Unix(55, 0),
			expRecord:       newThreeAssetExpRecord(poolId, twoDec.Add(OneSec.MulInt64(44*10)), oneDec.Add(OneSec.MulInt64(44).QuoInt64(10)), twoDec.Add(OneSec.MulInt64(44*20)), oneDec.Add(OneSec.MulInt64(44).Mul(logTen)), twoDec.Add(OneSec.MulInt64(44).Mul(logTen)), oneDec.Add(OneSec.MulInt64(44).Mul(logOneOverTen))),
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			for i := range test.record {
				// correct expected record based off copy/paste values
				test.expRecord[i].Time = test.interpolateTime
				test.expRecord[i].P0LastSpotPrice = test.record[i].P0LastSpotPrice
				test.expRecord[i].P1LastSpotPrice = test.record[i].P1LastSpotPrice

				gotRecord := twap.RecordWithUpdatedAccumulators(test.record[i], test.interpolateTime)
				require.Equal(t, test.expRecord[i], gotRecord)
			}
		})
	}
}

func (s *TestSuite) TestGetInterpolatedRecord() {
	baseRecord := newTwoAssetPoolTwapRecordWithDefaults(baseTime, sdk.OneDec(), sdk.OneDec(), sdk.OneDec(), sdk.OneDec().Quo(twoDec))

	// all tests occur with updateTime = base time + time.Unix(1, 0)
	tests := map[string]struct {
		recordsToPreSet     types.TwapRecord
		testPoolId          uint64
		testDenom0          string
		testDenom1          string
		testTime            time.Time
		expectedAccumulator sdk.Dec
		expectedErr         error
		expectedLastErrTime time.Time
	}{
		"same time with existing record": {
			recordsToPreSet: baseRecord,
			testPoolId:      baseRecord.PoolId,
			testDenom0:      baseRecord.Asset0Denom,
			testDenom1:      baseRecord.Asset1Denom,
			testTime:        baseTime,
		},
		"call 1 second after existing record": {
			recordsToPreSet: baseRecord,
			testPoolId:      baseRecord.PoolId,
			testDenom0:      baseRecord.Asset0Denom,
			testDenom1:      baseRecord.Asset1Denom,
			testTime:        baseTime.Add(time.Second),
			// 1(spot price) * 1000(one sec in milli-seconds)
			expectedAccumulator: baseRecord.P0ArithmeticTwapAccumulator.Add(sdk.NewDec(1000)),
		},
		"call 1 second after existing record with error": {
			recordsToPreSet: withLastErrTime(baseRecord, baseTime),
			testPoolId:      baseRecord.PoolId,
			testDenom0:      baseRecord.Asset0Denom,
			testDenom1:      baseRecord.Asset1Denom,
			testTime:        baseTime.Add(time.Second),
			// 1(spot price) * 1000(one sec in milli-seconds)
			expectedAccumulator: baseRecord.P0ArithmeticTwapAccumulator.Add(sdk.NewDec(1000)),
			expectedLastErrTime: baseTime.Add(time.Second),
		},
		"call 1 second before existing record": {
			recordsToPreSet: baseRecord,
			testPoolId:      baseRecord.PoolId,
			testDenom0:      baseRecord.Asset0Denom,
			testDenom1:      baseRecord.Asset1Denom,
			testTime:        baseTime.Add(-time.Second),
			expectedErr: fmt.Errorf("looking for a time thats too old, not in the historical index. "+
				" Try storing the accumulator value. (requested time %s)", baseTime.Add(-time.Second)),
		},
		"on lexicographical order denom parameters": {
			recordsToPreSet: baseRecord,
			testPoolId:      baseRecord.PoolId,
			testDenom0:      baseRecord.Asset0Denom,
			testDenom1:      baseRecord.Asset1Denom,
			testTime:        baseTime,
		},
		"test non lexicographical order parameter": {
			recordsToPreSet: baseRecord,
			testPoolId:      baseRecord.PoolId,
			testDenom0:      baseRecord.Asset1Denom,
			testDenom1:      baseRecord.Asset0Denom,
			testTime:        baseTime,
		},
	}

	for name, test := range tests {
		s.Run(name, func() {
			s.SetupTest()
			s.twapkeeper.StoreNewRecord(s.Ctx, test.recordsToPreSet)

			interpolatedRecord, err := s.twapkeeper.GetInterpolatedRecord(s.Ctx, test.testPoolId, test.testDenom0, test.testDenom1, test.testTime)
			if test.expectedErr != nil {
				s.Require().Error(err)
				s.Require().Equal(test.expectedErr.Error(), err.Error())
				return
			}
			s.Require().NoError(err)

			if test.testTime.Equal(baseTime) {
				s.Require().Equal(test.recordsToPreSet, interpolatedRecord)
			} else {
				s.Require().Equal(test.testTime, interpolatedRecord.Time)
				s.Require().Equal(test.recordsToPreSet.P0LastSpotPrice, interpolatedRecord.P0LastSpotPrice)
				s.Require().Equal(test.recordsToPreSet.P1LastSpotPrice, interpolatedRecord.P1LastSpotPrice)
				s.Require().Equal(test.expectedAccumulator, interpolatedRecord.P0ArithmeticTwapAccumulator)
				s.Require().Equal(test.expectedAccumulator, interpolatedRecord.P1ArithmeticTwapAccumulator)
				if test.recordsToPreSet.Time.Equal(test.recordsToPreSet.LastErrorTime) {
					// last error time updated
					s.Require().Equal(test.testTime, interpolatedRecord.LastErrorTime)
				} else {
					// last error time unchanged
					s.Require().Equal(test.recordsToPreSet.LastErrorTime, interpolatedRecord.LastErrorTime)
				}
			}
		})
	}
}

func (s *TestSuite) TestGetInterpolatedRecord_ThreeAsset() {
	baseRecord := newThreeAssetRecord(2, baseTime, sdk.NewDec(10), sdk.ZeroDec(), sdk.ZeroDec(), sdk.ZeroDec(), sdk.ZeroDec(), sdk.ZeroDec(), sdk.ZeroDec())
	// all tests occur with updateTime = base time + time.Unix(1, 0)
	tests := map[string]struct {
		recordsToPreSet       []types.TwapRecord
		testTime              time.Time
		expectedP0Accumulator []sdk.Dec
		expectedP1Accumulator []sdk.Dec
		expectedErr           error
	}{
		"call 1 second after existing record": {
			recordsToPreSet: baseRecord,
			testTime:        baseTime.Add(time.Second),
			// P0 and P1 TwapAccumulators both start at 0
			// A 10 spot price * 1000ms = 10000
			// A 10 spot price * 1000ms = 10000
			// B .1 spot price * 1000ms = 100
			expectedP0Accumulator: []sdk.Dec{
				baseRecord[0].P0ArithmeticTwapAccumulator.Add(sdk.NewDec(10000)),
				baseRecord[1].P0ArithmeticTwapAccumulator.Add(sdk.NewDec(10000)),
				baseRecord[2].P0ArithmeticTwapAccumulator.Add(sdk.NewDec(100)),
			},
			// B .1 spot price * 1000ms = 100
			// C 20 spot price * 1000ms = 20000
			// C 20 spot price * 1000ms = 20000
			expectedP1Accumulator: []sdk.Dec{
				baseRecord[0].P1ArithmeticTwapAccumulator.Add(sdk.NewDec(100)),
				baseRecord[1].P1ArithmeticTwapAccumulator.Add(sdk.NewDec(20000)),
				baseRecord[2].P1ArithmeticTwapAccumulator.Add(sdk.NewDec(20000)),
			},
		},
		"call 1 second after existing record with error": {
			recordsToPreSet: []types.TwapRecord{
				withLastErrTime(baseRecord[0], baseTime),
				withLastErrTime(baseRecord[1], baseTime),
				withLastErrTime(baseRecord[2], baseTime),
			},
			testTime: baseTime.Add(time.Second),
			// P0 and P1 TwapAccumulators both start at 0
			// A 10 spot price * 1000ms = 10000
			// A 10 spot price * 1000ms = 10000
			// B .1 spot price * 1000ms = 100
			expectedP0Accumulator: []sdk.Dec{
				baseRecord[0].P0ArithmeticTwapAccumulator.Add(sdk.NewDec(10000)),
				baseRecord[1].P0ArithmeticTwapAccumulator.Add(sdk.NewDec(10000)),
				baseRecord[2].P0ArithmeticTwapAccumulator.Add(sdk.NewDec(100)),
			},
			// B .1 spot price * 1000ms = 100
			// C 20 spot price * 1000ms = 20000
			// C 20 spot price * 1000ms = 20000
			expectedP1Accumulator: []sdk.Dec{
				baseRecord[0].P1ArithmeticTwapAccumulator.Add(sdk.NewDec(100)),
				baseRecord[1].P1ArithmeticTwapAccumulator.Add(sdk.NewDec(20000)),
				baseRecord[2].P1ArithmeticTwapAccumulator.Add(sdk.NewDec(20000)),
			},
		},
		"call 1 second before existing record": {
			recordsToPreSet: baseRecord,
			testTime:        baseTime.Add(-time.Second),
			expectedErr: fmt.Errorf("looking for a time thats too old, not in the historical index. "+
				" Try storing the accumulator value. (requested time %s)", baseTime.Add(-time.Second)),
		},
		"test non lexicographical order parameter": {
			recordsToPreSet: baseRecord,
			testTime:        baseTime,
		},
	}

	for name, test := range tests {
		s.Run(name, func() {
			s.SetupTest()
			for i := range test.recordsToPreSet {
				s.twapkeeper.StoreNewRecord(s.Ctx, test.recordsToPreSet[i])

				interpolatedRecord, err := s.twapkeeper.GetInterpolatedRecord(s.Ctx, baseRecord[i].PoolId, baseRecord[i].Asset0Denom, baseRecord[i].Asset1Denom, test.testTime)
				if test.expectedErr != nil {
					s.Require().Error(err)
					s.Require().Equal(test.expectedErr.Error(), err.Error())
					return
				}
				s.Require().NoError(err)

				if test.testTime.Equal(baseTime) {
					s.Require().Equal(test.recordsToPreSet[i], interpolatedRecord)
				} else {
					s.Require().Equal(test.testTime, interpolatedRecord.Time)
					s.Require().Equal(test.recordsToPreSet[i].P0LastSpotPrice, interpolatedRecord.P0LastSpotPrice)
					s.Require().Equal(test.recordsToPreSet[i].P1LastSpotPrice, interpolatedRecord.P1LastSpotPrice)
					s.Require().Equal(test.expectedP0Accumulator[i], interpolatedRecord.P0ArithmeticTwapAccumulator)
					s.Require().Equal(test.expectedP1Accumulator[i], interpolatedRecord.P1ArithmeticTwapAccumulator)
					if test.recordsToPreSet[i].Time.Equal(test.recordsToPreSet[i].LastErrorTime) {
						// last error time updated
						s.Require().Equal(test.testTime, interpolatedRecord.LastErrorTime)
					} else {
						// last error time unchanged
						s.Require().Equal(test.recordsToPreSet[i].LastErrorTime, interpolatedRecord.LastErrorTime)
					}
				}
			}
		})
	}
}

type computeTwapTestCase struct {
	startRecord types.TwapRecord
	endRecord   types.TwapRecord
	twapTypes   []twap.TwapType
	quoteAsset  string
	expTwap     sdk.Dec
	expErr      bool
	expPanic    bool
}

type computeThreeAssetArithmeticTwapTestCase struct {
	startRecord []types.TwapRecord
	endRecord   []types.TwapRecord
	quoteAsset  []string
	expTwap     []sdk.Dec
	expErr      bool
}

// TestComputeArithmeticTwap tests computeTwap on various inputs.
// TODO: test both arithmetic and geometric twap.
// The test vectors are structured by setting up different start and records,
// based on time interval, and their accumulator values.
// Then an expected TWAP is provided in each test case, to compare against computed.
func TestComputeTwap(t *testing.T) {
	tests := map[string]computeTwapTestCase{
		"arithmetic only, basic: spot price = 1 for one second, 0 init accumulator": {
			startRecord: newOneSidedRecord(baseTime, sdk.ZeroDec(), true),
			endRecord:   newOneSidedRecord(tPlusOne, OneSec, true),
			quoteAsset:  denom0,
			twapTypes:   []twap.TwapType{twap.ArithmeticTwapType},
			expTwap:     sdk.OneDec(),
		},
		// this test just shows what happens in case the records are reversed.
		// It should return the correct result, even though this is incorrect internal API usage
		"arithmetic only: invalid call: reversed records of above": {
			startRecord: newOneSidedRecord(tPlusOne, OneSec, true),
			endRecord:   newOneSidedRecord(baseTime, sdk.ZeroDec(), true),
			quoteAsset:  denom0,
			twapTypes:   []twap.TwapType{twap.ArithmeticTwapType},
			expTwap:     sdk.OneDec(),
		},
		"same record: denom0, end spot price = 0": {
			startRecord: newOneSidedRecord(baseTime, sdk.ZeroDec(), true),
			endRecord:   newOneSidedRecord(baseTime, sdk.ZeroDec(), true),
			quoteAsset:  denom0,
			twapTypes:   []twap.TwapType{twap.ArithmeticTwapType, twap.GeometricTwapType},
			expTwap:     sdk.ZeroDec(),
		},
		"same record: denom1, end spot price = 1": {
			startRecord: newOneSidedRecord(baseTime, sdk.ZeroDec(), true),
			endRecord:   newOneSidedRecord(baseTime, sdk.ZeroDec(), true),
			quoteAsset:  denom1,
			twapTypes:   []twap.TwapType{twap.ArithmeticTwapType, twap.GeometricTwapType},
			expTwap:     sdk.OneDec(),
		},
		"arithmetic only: accumulator = 10*OneSec, t=5s. 0 base accum": testCaseFromDeltas(
			sdk.ZeroDec(), tenSecAccum, 5*time.Second, sdk.NewDec(2)),
		"arithmetic only: accumulator = 10*OneSec, t=100s. 0 base accum (asset 1)": testCaseFromDeltasAsset1(sdk.ZeroDec(), OneSec.MulInt64(10), 100*time.Second, sdk.NewDecWithPrec(1, 1)),
		"geometric only: accumulator = log(10)*OneSec, t=5s. 0 base accum": geometricTestCaseFromDeltas0(
			sdk.ZeroDec(), geometricTenSecAccum, 5*time.Second, twap.TwapPow(geometricTenSecAccum.QuoInt64(5*1000))),
		"geometric only: accumulator = log(10)*OneSec, t=100s. 0 base accum (asset 1)": geometricTestCaseFromDeltas1(sdk.ZeroDec(), geometricTenSecAccum, 100*time.Second, sdk.OneDec().Quo(twap.TwapPow(geometricTenSecAccum.QuoInt64(100*1000)))),
	}
	for name, test := range tests {
		for _, twapType := range test.twapTypes {
			twapType := twapType
			twapTypeStr := "arithmetic"
			if twapType == twap.GeometricTwapType {
				twapTypeStr = "geometric"
			}

			t.Run(fmt.Sprintf("%s - %s", twapTypeStr, name), func(t *testing.T) {
				actualTwap, err := twap.ComputeTwap(test.startRecord, test.endRecord, test.quoteAsset, twapType)
				require.NoError(t, err)
				osmoassert.DecApproxEq(t, test.expTwap, actualTwap, osmomath.GetPowPrecision())
			})
		}
	}
}

// TestComputeArithmeticTwap tests computeArithmeticTwap on various inputs.
// Contrary to computeTwap that handles the cases with zero delta correctly,
// this function should panic in case of zero delta.
func TestComputeArithmeticTwap(t *testing.T) {
	pointOneAccum := OneSec.QuoInt64(10)
	tests := map[string]computeTwapTestCase{
		"basic: spot price = 1 for one second, 0 init accumulator": {
			startRecord: newOneSidedRecord(baseTime, sdk.ZeroDec(), true),
			endRecord:   newOneSidedRecord(tPlusOne, OneSec, true),
			quoteAsset:  denom0,
			expTwap:     sdk.OneDec(),
		},
		// this test just shows what happens in case the records are reversed.
		// It should return the correct result, even though this is incorrect internal API usage
		"invalid call: reversed records of above": {
			startRecord: newOneSidedRecord(tPlusOne, OneSec, true),
			endRecord:   newOneSidedRecord(baseTime, sdk.ZeroDec(), true),
			quoteAsset:  denom0,
			expTwap:     sdk.OneDec(),
		},
		"same record (zero time delta), division by 0 - panic": {
			startRecord: newOneSidedRecord(baseTime, sdk.ZeroDec(), true),
			endRecord:   newOneSidedRecord(baseTime, sdk.ZeroDec(), true),
			quoteAsset:  denom0,
			expPanic:    true,
		},
		"accumulator = 10*OneSec, t=5s. 0 base accum": testCaseFromDeltas(
			sdk.ZeroDec(), tenSecAccum, 5*time.Second, sdk.NewDec(2)),
		"accumulator = 10*OneSec, t=3s. 0 base accum": testCaseFromDeltas(
			sdk.ZeroDec(), tenSecAccum, 3*time.Second, ThreePlusOneThird),
		"accumulator = 10*OneSec, t=100s. 0 base accum": testCaseFromDeltas(
			sdk.ZeroDec(), tenSecAccum, 100*time.Second, sdk.NewDecWithPrec(1, 1)),

		// test that base accum has no impact
		"accumulator = 10*OneSec, t=5s. 10 base accum": testCaseFromDeltas(
			sdk.NewDec(10), tenSecAccum, 5*time.Second, sdk.NewDec(2)),
		"accumulator = 10*OneSec, t=3s. 10*second base accum": testCaseFromDeltas(
			tenSecAccum, tenSecAccum, 3*time.Second, ThreePlusOneThird),
		"accumulator = 10*OneSec, t=100s. .1*second base accum": testCaseFromDeltas(
			pointOneAccum, tenSecAccum, 100*time.Second, sdk.NewDecWithPrec(1, 1)),

		"accumulator = 10*OneSec, t=100s. 0 base accum (asset 1)": testCaseFromDeltasAsset1(sdk.ZeroDec(), OneSec.MulInt64(10), 100*time.Second, sdk.NewDecWithPrec(1, 1)),
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {

			osmoassert.ConditionalPanic(t, test.expPanic, func() {
				actualTwap := twap.ComputeArithmeticTwap(test.startRecord, test.endRecord, test.quoteAsset)
				require.Equal(t, test.expTwap, actualTwap)
			})
		})
	}
}

func TestComputeGeometricTwap(t *testing.T) {
	tests := map[string]computeTwapTestCase{
		// basic test for both denom with zero start accumulator
		"basic denom0: spot price = 1 for one second, 0 init accumulator": {
			startRecord: newOneSidedGeometricRecord(baseTime, sdk.ZeroDec()),
			endRecord:   newOneSidedGeometricRecord(tPlusOne, geometricTenSecAccum),
			quoteAsset:  denom0,
			expTwap:     sdk.NewDec(10),
		},
		"basic denom1: spot price = 1 for one second, 0 init accumulator": {
			startRecord: newOneSidedGeometricRecord(baseTime, sdk.ZeroDec()),
			endRecord:   newOneSidedGeometricRecord(tPlusOne, geometricTenSecAccum),
			quoteAsset:  denom1,
			expTwap:     sdk.OneDec().Quo(sdk.NewDec(10)),
		},

		// basic test for both denom with non-zero start accumulator
		"denom0: start accumulator of 10 * 1s, end accumulator 10 * 1s + 20 * 2s = 20": {
			startRecord: newOneSidedGeometricRecord(baseTime, geometricTenSecAccum),
			endRecord:   newOneSidedGeometricRecord(baseTime.Add(time.Second*2), geometricTenSecAccum.Add(OneSec.MulInt64(2).Mul(twap.TwapLog(sdk.NewDec(20))))),
			quoteAsset:  denom0,
			expTwap:     sdk.NewDec(20),
		},
		"denom1 start accumulator of 10 * 1s, end accumulator 10 * 1s + 20 * 2s = 20": {
			startRecord: newOneSidedGeometricRecord(baseTime, geometricTenSecAccum),
			endRecord:   newOneSidedGeometricRecord(baseTime.Add(time.Second*2), geometricTenSecAccum.Add(OneSec.MulInt64(2).Mul(twap.TwapLog(sdk.NewDec(20))))),
			quoteAsset:  denom1,
			expTwap:     sdk.OneDec().Quo(sdk.NewDec(20)),
		},

		// toggle time delta.
		"accumulator = log(10)*OneSec, t=5s. 0 base accum": geometricTestCaseFromDeltas0(
			sdk.ZeroDec(), geometricTenSecAccum, 5*time.Second, twap.TwapPow(geometricTenSecAccum.QuoInt64(5*1000))),
		"accumulator = log(10)*OneSec, t=3s. 0 base accum": geometricTestCaseFromDeltas0(
			sdk.ZeroDec(), geometricTenSecAccum, 3*time.Second, twap.TwapPow(geometricTenSecAccum.QuoInt64(3*1000))),
		"accumulator = log(10)*OneSec, t=100s. 0 base accum": geometricTestCaseFromDeltas0(
			sdk.ZeroDec(), geometricTenSecAccum, 100*time.Second, twap.TwapPow(geometricTenSecAccum.QuoInt64(100*1000))),

		// test that base accum has no impact
		"accumulator = log(10)*OneSec, t=5s. 10 base accum": geometricTestCaseFromDeltas0(
			logTen, geometricTenSecAccum, 5*time.Second, twap.TwapPow(geometricTenSecAccum.QuoInt64(5*1000))),
		"accumulator = log(10)*OneSec, t=3s. 10*second base accum": geometricTestCaseFromDeltas0(
			OneSec.MulInt64(10).Mul(logTen), geometricTenSecAccum, 3*time.Second, twap.TwapPow(geometricTenSecAccum.QuoInt64(3*1000))),
		"accumulator = 10*OneSec, t=100s. .1*second base accum": geometricTestCaseFromDeltas0(
			OneSec.MulInt64(10).Mul(logOneOverTen), geometricTenSecAccum, 100*time.Second, twap.TwapPow(geometricTenSecAccum.QuoInt64(100*1000))),

		// TODO: this is the highest price we currently support with the given precision bounds.
		// Need to choose better base and potentially improve math functions to mitigate.
		"price of 1_000_000 for an hour": {
			startRecord: newOneSidedGeometricRecord(baseTime, sdk.ZeroDec()),
			endRecord:   newOneSidedGeometricRecord(baseTime.Add(time.Hour), OneSec.MulInt64(60*60).Mul(twap.TwapLog(sdk.NewDec(1_000_000)))),
			quoteAsset:  denom0,
			expTwap:     sdk.NewDec(1_000_000),
		},
		// TODO: overflow tests
		// - max spot price
		// - large time delta
		// - both

		// TODO: hand calculated tests
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			osmoassert.ConditionalPanic(t, tc.expPanic, func() {
				actualTwap := twap.ComputeGeometricTwap(tc.startRecord, tc.endRecord, tc.quoteAsset)
				osmoassert.DecApproxEq(t, tc.expTwap, actualTwap, osmomath.GetPowPrecision())
			})
		})
	}
}

func TestComputeArithmeticTwap_ThreeAsset_Arithmetic(t *testing.T) {
	tenSecAccum := OneSec.MulInt64(10)
	pointOneAccum := OneSec.QuoInt64(10)
	tests := map[string]computeThreeAssetArithmeticTwapTestCase{
		"three asset basic: spot price = 1 for one second, 0 init accumulator": {
			startRecord: newThreeAssetOneSidedRecord(baseTime, sdk.ZeroDec(), true),
			endRecord:   newThreeAssetOneSidedRecord(tPlusOne, OneSec, true),
			quoteAsset:  []string{denom0, denom0, denom1},
			expTwap:     []sdk.Dec{sdk.OneDec(), sdk.OneDec(), sdk.OneDec()},
		},
		"three asset same record: asset1, end spot price = 1": {
			startRecord: newThreeAssetOneSidedRecord(baseTime, sdk.ZeroDec(), true),
			endRecord:   newThreeAssetOneSidedRecord(baseTime, sdk.ZeroDec(), true),
			quoteAsset:  []string{denom1, denom2, denom2},
			expTwap:     []sdk.Dec{sdk.OneDec(), sdk.OneDec(), sdk.OneDec()},
		},
		"three asset. accumulator = 10*OneSec, t=5s. 0 base accum": testThreeAssetCaseFromDeltas(
			sdk.ZeroDec(), tenSecAccum, 5*time.Second, sdk.NewDec(2)),

		// test that base accum has no impact
		"three asset. accumulator = 10*OneSec, t=5s. 10 base accum": testThreeAssetCaseFromDeltas(
			sdk.NewDec(10), tenSecAccum, 5*time.Second, sdk.NewDec(2)),
		"three asset. accumulator = 10*OneSec, t=100s. .1*second base accum": testThreeAssetCaseFromDeltas(
			pointOneAccum, tenSecAccum, 100*time.Second, sdk.NewDecWithPrec(1, 1)),
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			for i, startRec := range test.startRecord {
				actualTwap, err := twap.ComputeTwap(startRec, test.endRecord[i], test.quoteAsset[i], twap.ArithmeticTwapType)
				require.Equal(t, test.expTwap[i], actualTwap)
				require.NoError(t, err)
			}
		})
	}
}

func TestComputeArithmeticTwap_ThreeAsset_Geometric(t *testing.T) {
	var (
		five        = sdk.NewDec(5)
		fiveFor3Sec = OneSec.MulInt64(3).Mul(twap.TwapLog(five))

		ten          = five.MulInt64(2)
		tenFor100Sec = OneSec.MulInt64(100).Mul(twap.TwapLog(ten))

		errTolerance = sdk.MustNewDecFromStr("0.00000001")
	)

	tests := map[string]computeThreeAssetArithmeticTwapTestCase{
		"three asset basic: spot price = 10 for one second, 0 init accumulator": {
			startRecord: newThreeAssetOneSidedRecord(baseTime, sdk.ZeroDec(), true),
			endRecord:   newThreeAssetOneSidedRecord(tPlusOne, geometricTenSecAccum, true),
			quoteAsset:  []string{denom0, denom0, denom1},
			expTwap:     []sdk.Dec{sdk.NewDec(10), sdk.NewDec(10), sdk.NewDec(10)},
		},
		"three asset same record: asset1, end spot price = 1": {
			startRecord: newThreeAssetOneSidedRecord(baseTime, sdk.ZeroDec(), true),
			endRecord:   newThreeAssetOneSidedRecord(baseTime, sdk.ZeroDec(), true),
			quoteAsset:  []string{denom1, denom2, denom2},
			expTwap:     []sdk.Dec{sdk.OneDec(), sdk.OneDec(), sdk.OneDec()},
		},
		"three asset. accumulator = 5*3Sec, t=3s, no start accum": testThreeAssetCaseFromDeltas(
			sdk.ZeroDec(), fiveFor3Sec, 3*time.Second, five),

		// test that base accum has no impact
		"three asset. accumulator = 5*3Sec, t=3s. 10 base accum": testThreeAssetCaseFromDeltas(
			geometricTenSecAccum, fiveFor3Sec, 3*time.Second, five),
		"three asset. accumulator = 100*100s, t=100s. .1*second base accum": testThreeAssetCaseFromDeltas(
			twap.TwapLog(OneSec.Quo(ten)), tenFor100Sec, 100*time.Second, ten),
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			for i, startRec := range test.startRecord {
				actualTwap, err := twap.ComputeTwap(startRec, test.endRecord[i], test.quoteAsset[i], twap.GeometricTwapType)

				require.NoError(t, err)
				osmoassert.DecApproxEq(t, test.expTwap[i], actualTwap, errTolerance)
			}
		})
	}
}

// This tests the behavior of computeArithmeticTwap, around error returning
// when there has been an intermediate spot price error.
func TestComputeArithmeticTwapWithSpotPriceError(t *testing.T) {
	newOneSidedRecordWErrorTime := func(time time.Time, accum sdk.Dec, useP0 bool, errTime time.Time) types.TwapRecord {
		record := newOneSidedRecord(time, accum, useP0)
		record.LastErrorTime = errTime
		return record
	}
	tests := map[string]computeTwapTestCase{
		// should error, since end time may have been used to interpolate this value
		"errAtEndTime from end record": {
			startRecord: newOneSidedRecord(baseTime, sdk.ZeroDec(), true),
			endRecord:   newOneSidedRecordWErrorTime(tPlusOne, OneSec, true, tPlusOne),
			quoteAsset:  denom0,
			expTwap:     sdk.OneDec(),
			expErr:      true,
		},
		// should error, since start time may have been used to interpolate this value
		"err at StartTime exactly from end record": {
			startRecord: newOneSidedRecord(baseTime, sdk.ZeroDec(), true),
			endRecord:   newOneSidedRecordWErrorTime(tPlusOne, OneSec, true, baseTime),
			quoteAsset:  denom0,
			expTwap:     sdk.OneDec(),
			expErr:      true,
		},
		// should error, since start record is erroneous
		"err at StartTime exactly from start record": {
			startRecord: newOneSidedRecordWErrorTime(baseTime, sdk.ZeroDec(), true, baseTime),
			endRecord:   newOneSidedRecord(tPlusOne, OneSec, true),
			quoteAsset:  denom0,
			expTwap:     sdk.OneDec(),
			expErr:      true,
		},
		"err before StartTime": {
			startRecord: newOneSidedRecord(baseTime, sdk.ZeroDec(), true),
			endRecord:   newOneSidedRecordWErrorTime(tPlusOne, OneSec, true, tMinOne),
			quoteAsset:  denom0,
			expTwap:     sdk.OneDec(),
			expErr:      false,
		},
		// Should not happen, but if it did would error
		"err after EndTime": {
			startRecord: newOneSidedRecord(baseTime, sdk.ZeroDec(), true),
			endRecord:   newOneSidedRecordWErrorTime(tPlusOne, OneSec.MulInt64(2), true, baseTime.Add(20*time.Second)),
			quoteAsset:  denom0,
			expTwap:     sdk.OneDec().MulInt64(2),
			expErr:      true,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			actualTwap, err := twap.ComputeTwap(test.startRecord, test.endRecord, test.quoteAsset, twap.ArithmeticTwapType)
			require.Equal(t, test.expTwap, actualTwap)
			osmoassert.ConditionalError(t, test.expErr, err)
		})
	}
}

// TestPruneRecords tests that twap records earlier than
// current block time - RecordHistoryKeepPeriod are pruned from the store
// while keeping the newest record before the above time threshold.
// Such record is kept for each pool.
func (s *TestSuite) TestPruneRecords() {
	recordHistoryKeepPeriod := s.twapkeeper.RecordHistoryKeepPeriod(s.Ctx)

	pool1OlderMin2MsRecord, // deleted
		pool2OlderMin1MsRecordAB, pool2OlderMin1MsRecordAC, pool2OlderMin1MsRecordBC, // deleted
		pool3OlderBaseRecord,    // kept as newest under keep period
		pool4OlderPlus1Record := // kept as newest under keep period
		s.createTestRecordsFromTime(baseTime.Add(2 * -recordHistoryKeepPeriod))

	pool1Min2MsRecord, // kept as newest under keep period
		pool2Min1MsRecordAB, pool2Min1MsRecordAC, pool2Min1MsRecordBC, // kept as newest under keep period
		pool3BaseRecord,    // kept as it is at the keep period boundary
		pool4Plus1Record := // kept as it is above the keep period boundary
		s.createTestRecordsFromTime(baseTime.Add(-recordHistoryKeepPeriod))

	// non-ascending insertion order.
	recordsToPreSet := []types.TwapRecord{
		pool2OlderMin1MsRecordAB, pool2OlderMin1MsRecordAC, pool2OlderMin1MsRecordBC,
		pool4Plus1Record,
		pool4OlderPlus1Record,
		pool3OlderBaseRecord,
		pool2Min1MsRecordAB, pool2Min1MsRecordAC, pool2Min1MsRecordBC,
		pool3BaseRecord,
		pool1Min2MsRecord,
		pool1OlderMin2MsRecord,
	}

	// tMin2Record is before the threshold and is pruned away.
	// tmin1Record is the newest record before current block time - record history keep period.
	// All other records happen after the threshold and are kept.
	expectedKeptRecords := []types.TwapRecord{
		pool3OlderBaseRecord,
		pool4OlderPlus1Record,
		pool1Min2MsRecord,
		pool2Min1MsRecordAB, pool2Min1MsRecordAC, pool2Min1MsRecordBC,
		pool3BaseRecord,
		pool4Plus1Record,
	}
	s.SetupTest()
	s.preSetRecords(recordsToPreSet)

	ctx := s.Ctx
	twapKeeper := s.twapkeeper

	ctx = ctx.WithBlockTime(baseTime)

	err := twapKeeper.PruneRecords(ctx)
	s.Require().NoError(err)

	s.validateExpectedRecords(expectedKeptRecords)
}

// TestUpdateRecords tests that the records are updated correctly.
// It tests the following:
// - two-asset pools
// - multi-asset pools
// - with spot price errors
// - without spot price errors
// - that new records are created
// - older historical records are not updated
// - spot price error times are either propagated from
// older records or set to current block time in case error occurred.
func (s *TestSuite) TestUpdateRecords() {
	type spOverride struct {
		poolId      uint64
		baseDenom   string
		quoteDenom  string
		overrideSp  sdk.Dec
		overrideErr error
	}

	type expectedResults struct {
		spotPriceA    sdk.Dec
		spotPriceB    sdk.Dec
		lastErrorTime time.Time
		isMostRecent  bool
	}

	spError := errors.New("spot price error")

	validateRecords := func(expectedRecords []expectedResults, actualRecords []types.TwapRecord) {
		s.Require().Equal(len(expectedRecords), len(actualRecords))
		for i, r := range expectedRecords {
			s.Require().Equal(r.spotPriceA, actualRecords[i].P0LastSpotPrice, "record %d", i)
			s.Require().Equal(r.spotPriceB, actualRecords[i].P1LastSpotPrice, "record %d", i)
			s.Require().Equal(r.lastErrorTime, actualRecords[i].LastErrorTime, "record %d", i)
		}
	}

	tests := map[string]struct {
		preSetRecords []types.TwapRecord
		poolId        uint64
		ammMock       twapmock.ProgrammedAmmInterface
		spOverrides   []spOverride
		blockTime     time.Time

		expectedHistoricalRecords []expectedResults
		expectError               error
	}{
		"no records pre-set; error": {
			preSetRecords: []types.TwapRecord{},
			poolId:        1,
			blockTime:     baseTime,

			expectError: gammtypes.PoolDoesNotExistError{PoolId: 1},
		},
		"existing records in different pool; no-op": {
			preSetRecords: []types.TwapRecord{baseRecord},
			poolId:        baseRecord.PoolId + 1,
			blockTime:     baseTime.Add(time.Second),

			expectError: gammtypes.PoolDoesNotExistError{PoolId: baseRecord.PoolId + 1},
		},
		"the returned number of records does not match expected": {
			preSetRecords: []types.TwapRecord{baseRecord},
			poolId:        baseRecord.PoolId,
			blockTime:     baseRecord.Time.Add(time.Second),

			spOverrides: []spOverride{
				{
					baseDenom:  baseRecord.Asset0Denom,
					quoteDenom: baseRecord.Asset1Denom,
					overrideSp: sdk.NewDec(2),
				},
				{
					baseDenom:  baseRecord.Asset1Denom,
					quoteDenom: baseRecord.Asset0Denom,
					overrideSp: sdk.NewDecWithPrec(2, 1),
				},
				{
					baseDenom:  baseRecord.Asset1Denom,
					quoteDenom: "extradenom",
					overrideSp: sdk.NewDecWithPrec(3, 1),
				},
			},

			expectError: types.InvalidRecordCountError{Expected: 3, Actual: 1},
		},
		"two-asset; pre-set record at t; updated valid spot price": {
			preSetRecords: []types.TwapRecord{baseRecord},
			poolId:        baseRecord.PoolId,
			blockTime:     baseRecord.Time.Add(time.Second),

			spOverrides: []spOverride{
				{
					baseDenom:  baseRecord.Asset0Denom,
					quoteDenom: baseRecord.Asset1Denom,
					overrideSp: sdk.NewDec(2),
				},
				{
					baseDenom:  baseRecord.Asset1Denom,
					quoteDenom: baseRecord.Asset0Denom,
					overrideSp: sdk.NewDecWithPrec(2, 1),
				},
			},

			expectedHistoricalRecords: []expectedResults{
				// The original record.
				{
					spotPriceA: baseRecord.P0LastSpotPrice,
					spotPriceB: baseRecord.P1LastSpotPrice,
				},
				// The new record added.
				{
					spotPriceA:   sdk.NewDec(2),
					spotPriceB:   sdk.NewDecWithPrec(2, 1),
					isMostRecent: true,
				},
			},
		},
		"two-asset; pre-set record at t; updated with spot price error in both denom pairs": {
			preSetRecords: []types.TwapRecord{baseRecord},
			poolId:        baseRecord.PoolId,
			blockTime:     baseRecord.Time.Add(time.Second),

			spOverrides: []spOverride{
				{
					baseDenom:   baseRecord.Asset0Denom,
					quoteDenom:  baseRecord.Asset1Denom,
					overrideErr: spError,
				},
				{
					baseDenom:   baseRecord.Asset1Denom,
					quoteDenom:  baseRecord.Asset0Denom,
					overrideSp:  sdk.NewDecWithPrec(2, 1),
					overrideErr: spError,
				},
			},

			expectedHistoricalRecords: []expectedResults{
				// The original record.
				{
					spotPriceA: baseRecord.P0LastSpotPrice,
					spotPriceB: baseRecord.P1LastSpotPrice,
				},
				// The new record added.
				{
					spotPriceA:    sdk.ZeroDec(),
					spotPriceB:    sdk.NewDecWithPrec(2, 1),
					lastErrorTime: baseRecord.Time.Add(time.Second), // equals to block time
					isMostRecent:  true,
				},
			},
		},
		"two-asset; pre-set record at t; large spot price in one of the pairs": {
			preSetRecords: []types.TwapRecord{baseRecord},
			poolId:        baseRecord.PoolId,
			blockTime:     baseRecord.Time.Add(time.Second),

			spOverrides: []spOverride{
				{
					baseDenom:  baseRecord.Asset0Denom,
					quoteDenom: baseRecord.Asset1Denom,
					overrideSp: sdk.OneDec(),
				},
				{
					baseDenom:   baseRecord.Asset1Denom,
					quoteDenom:  baseRecord.Asset0Denom,
					overrideSp:  types.MaxSpotPrice.Add(sdk.OneDec()),
					overrideErr: nil, // twap logic should identify the large spot price and mark it as error.
				},
			},

			expectedHistoricalRecords: []expectedResults{
				// The original record.
				{
					spotPriceA: baseRecord.P0LastSpotPrice,
					spotPriceB: baseRecord.P1LastSpotPrice,
				},
				// The new record added.
				{
					spotPriceA:    sdk.OneDec(),
					spotPriceB:    types.MaxSpotPrice,               // Although the price returned from AMM was MaxSpotPrice + 1, it is reset to just MaxSpotPrice.
					lastErrorTime: baseRecord.Time.Add(time.Second), // equals to block time
					isMostRecent:  true,
				},
			},
		},
		"two-asset; pre-set record at t with sp error; new record with no sp error; new record has old sp error": {
			preSetRecords: []types.TwapRecord{withLastErrTime(baseRecord, baseRecord.Time)},
			poolId:        baseRecord.PoolId,
			blockTime:     baseRecord.Time.Add(time.Second),

			spOverrides: []spOverride{
				{
					baseDenom:  baseRecord.Asset0Denom,
					quoteDenom: baseRecord.Asset1Denom,
					overrideSp: sdk.OneDec(),
				},
				{
					baseDenom:  baseRecord.Asset1Denom,
					quoteDenom: baseRecord.Asset0Denom,
					overrideSp: sdk.OneDec(),
				},
			},

			expectedHistoricalRecords: []expectedResults{
				// The original record.
				{
					spotPriceA:    baseRecord.P0LastSpotPrice,
					spotPriceB:    baseRecord.P1LastSpotPrice,
					lastErrorTime: baseRecord.Time,
				},
				// The new record added.
				{
					spotPriceA:    sdk.OneDec(),
					spotPriceB:    sdk.OneDec(),
					lastErrorTime: baseRecord.Time,
					isMostRecent:  true,
				},
			},
		},
		"two-asset; pre-set record at t with sp error; new record with sp error and has its sp err time updated": {
			preSetRecords: []types.TwapRecord{withLastErrTime(baseRecord, baseRecord.Time)},
			poolId:        baseRecord.PoolId,
			blockTime:     baseRecord.Time.Add(time.Second),

			spOverrides: []spOverride{
				{
					baseDenom:  baseRecord.Asset0Denom,
					quoteDenom: baseRecord.Asset1Denom,
					overrideSp: sdk.OneDec(),
				},
				{
					baseDenom:   baseRecord.Asset1Denom,
					quoteDenom:  baseRecord.Asset0Denom,
					overrideErr: spError,
				},
			},

			expectedHistoricalRecords: []expectedResults{
				// The original record.
				{
					spotPriceA:    baseRecord.P0LastSpotPrice,
					spotPriceB:    baseRecord.P1LastSpotPrice,
					lastErrorTime: baseRecord.Time,
				},
				// The new record added.
				{
					spotPriceA:    sdk.OneDec(),
					spotPriceB:    sdk.ZeroDec(),
					lastErrorTime: baseRecord.Time.Add(time.Second), // equals to block time
					isMostRecent:  true,
				},
			},
		},
		"two-asset; pre-set at t and t + 1, new record with updated spot price created": {
			preSetRecords: []types.TwapRecord{baseRecord, tPlus10sp5Record},
			poolId:        baseRecord.PoolId,

			blockTime: baseRecord.Time.Add(time.Second * 11),

			spOverrides: []spOverride{
				{
					baseDenom:  baseRecord.Asset0Denom,
					quoteDenom: baseRecord.Asset1Denom,
					overrideSp: sdk.OneDec(),
				},
				{
					baseDenom:  baseRecord.Asset1Denom,
					quoteDenom: baseRecord.Asset0Denom,
					overrideSp: sdk.OneDec().Add(sdk.OneDec()),
				},
			},

			expectedHistoricalRecords: []expectedResults{
				// The original record at t.
				{
					spotPriceA: baseRecord.P0LastSpotPrice,
					spotPriceB: baseRecord.P1LastSpotPrice,
				},
				// The original record at t + 1.
				{
					spotPriceA: tPlus10sp5Record.P0LastSpotPrice,
					spotPriceB: tPlus10sp5Record.P1LastSpotPrice,
				},
				// The new record added.
				{
					spotPriceA:   sdk.OneDec(),
					spotPriceB:   sdk.OneDec().Add(sdk.OneDec()),
					isMostRecent: true,
				},
			},
		},
		// This case should never happen in-practice since ctx.BlockTime
		// should always be greater than the last record's time.
		"two-asset; pre-set at t and t + 1, new record inserted between existing": {
			preSetRecords: []types.TwapRecord{baseRecord, tPlus10sp5Record},
			poolId:        baseRecord.PoolId,

			blockTime: baseRecord.Time.Add(time.Second * 5),

			spOverrides: []spOverride{
				{
					baseDenom:  baseRecord.Asset0Denom,
					quoteDenom: baseRecord.Asset1Denom,
					overrideSp: sdk.OneDec(),
				},
				{
					baseDenom:  baseRecord.Asset1Denom,
					quoteDenom: baseRecord.Asset0Denom,
					overrideSp: sdk.OneDec().Add(sdk.OneDec()),
				},
			},

			expectedHistoricalRecords: []expectedResults{
				// The original record at t.
				{
					spotPriceA: baseRecord.P0LastSpotPrice,
					spotPriceB: baseRecord.P1LastSpotPrice,
				},
				// The new record added.
				// TODO: it should not be possible to add a record between existing records.
				// https://github.com/osmosis-labs/osmosis/issues/2686
				{
					spotPriceA:   sdk.OneDec(),
					spotPriceB:   sdk.OneDec().Add(sdk.OneDec()),
					isMostRecent: true,
				},
				// The original record at t + 1.
				{
					spotPriceA: tPlus10sp5Record.P0LastSpotPrice,
					spotPriceB: tPlus10sp5Record.P1LastSpotPrice,
				},
			},
		},
		"multi-asset pool; pre-set at t and t + 1; creates new records": {
			preSetRecords: []types.TwapRecord{threeAssetRecordAB, threeAssetRecordAC, threeAssetRecordBC, tPlus10sp5ThreeAssetRecordAB, tPlus10sp5ThreeAssetRecordAC, tPlus10sp5ThreeAssetRecordBC},
			poolId:        threeAssetRecordAB.PoolId,
			blockTime:     threeAssetRecordAB.Time.Add(time.Second * 11),
			spOverrides: []spOverride{
				{
					baseDenom:  threeAssetRecordAB.Asset0Denom,
					quoteDenom: threeAssetRecordAB.Asset1Denom,
					overrideSp: sdk.OneDec(),
				},
				{
					baseDenom:  threeAssetRecordAB.Asset1Denom,
					quoteDenom: threeAssetRecordAB.Asset0Denom,
					overrideSp: sdk.OneDec().Add(sdk.OneDec()),
				},
				{
					baseDenom:  threeAssetRecordAC.Asset0Denom,
					quoteDenom: threeAssetRecordAC.Asset1Denom,
					overrideSp: sdk.OneDec(),
				},
				{
					baseDenom:  threeAssetRecordAC.Asset1Denom,
					quoteDenom: threeAssetRecordAC.Asset0Denom,
					overrideSp: sdk.OneDec().Add(sdk.OneDec()).Add(sdk.OneDec()),
				},
				{
					baseDenom:  threeAssetRecordBC.Asset0Denom,
					quoteDenom: threeAssetRecordBC.Asset1Denom,
					overrideSp: sdk.OneDec().Add(sdk.OneDec()),
				},
				{
					baseDenom:  threeAssetRecordBC.Asset1Denom,
					quoteDenom: threeAssetRecordBC.Asset0Denom,
					overrideSp: sdk.OneDec(),
				},
			},

			expectedHistoricalRecords: []expectedResults{
				// The original record AB at t.
				{
					spotPriceA: threeAssetRecordAB.P0LastSpotPrice,
					spotPriceB: threeAssetRecordAB.P1LastSpotPrice,
				},
				// The original record AB at t + 1.
				{
					spotPriceA: tPlus10sp5ThreeAssetRecordAB.P0LastSpotPrice,
					spotPriceB: tPlus10sp5ThreeAssetRecordAB.P1LastSpotPrice,
				},
				// The new record AB added.
				{
					spotPriceA:   sdk.OneDec(),
					spotPriceB:   sdk.OneDec().Add(sdk.OneDec()),
					isMostRecent: true,
				},
				// The original record AC at t.
				{
					spotPriceA: threeAssetRecordAC.P0LastSpotPrice,
					spotPriceB: threeAssetRecordAC.P1LastSpotPrice,
				},
				// The original record AC at t + 1.
				{
					spotPriceA: tPlus10sp5ThreeAssetRecordAC.P0LastSpotPrice,
					spotPriceB: tPlus10sp5ThreeAssetRecordAC.P1LastSpotPrice,
				},
				// The new record AC added.
				{
					spotPriceA:   sdk.OneDec(),
					spotPriceB:   sdk.OneDec().Add(sdk.OneDec()).Add(sdk.OneDec()),
					isMostRecent: true,
				},
				// The original record BC at t.
				{
					spotPriceA: threeAssetRecordBC.P0LastSpotPrice,
					spotPriceB: threeAssetRecordBC.P1LastSpotPrice,
				},
				// The original record BC at t + 1.
				{
					spotPriceA: tPlus10sp5ThreeAssetRecordBC.P0LastSpotPrice,
					spotPriceB: tPlus10sp5ThreeAssetRecordBC.P1LastSpotPrice,
				},
				// The new record BC added.
				{
					spotPriceA:   sdk.OneDec().Add(sdk.OneDec()),
					spotPriceB:   sdk.OneDec(),
					isMostRecent: true,
				},
			},
		},
		"multi-asset pool; pre-set at t and t + 1 with err, large spot price, overwrites error time": {
			preSetRecords: []types.TwapRecord{threeAssetRecordAB, threeAssetRecordAC, threeAssetRecordBC, withLastErrTime(tPlus10sp5ThreeAssetRecordAB, tPlus10sp5ThreeAssetRecordAB.Time), tPlus10sp5ThreeAssetRecordAC, tPlus10sp5ThreeAssetRecordBC},
			poolId:        threeAssetRecordAB.PoolId,
			blockTime:     threeAssetRecordAB.Time.Add(time.Second * 11),
			spOverrides: []spOverride{
				{
					baseDenom:  threeAssetRecordAB.Asset0Denom,
					quoteDenom: threeAssetRecordAB.Asset1Denom,
					overrideSp: sdk.OneDec(),
				},
				{
					baseDenom:  threeAssetRecordAB.Asset1Denom,
					quoteDenom: threeAssetRecordAB.Asset0Denom,
					overrideSp: sdk.OneDec().Add(sdk.OneDec()),
				},
				{
					baseDenom:  threeAssetRecordAC.Asset0Denom,
					quoteDenom: threeAssetRecordAC.Asset1Denom,
					overrideSp: sdk.OneDec(),
				},
				{
					baseDenom:   threeAssetRecordAC.Asset1Denom,
					quoteDenom:  threeAssetRecordAC.Asset0Denom,
					overrideSp:  types.MaxSpotPrice.Add(sdk.OneDec()),
					overrideErr: nil, // twap logic should identify the large spot price and mark it as error.
				},
				{
					baseDenom:  threeAssetRecordBC.Asset0Denom,
					quoteDenom: threeAssetRecordBC.Asset1Denom,
					overrideSp: sdk.OneDec(),
				},
				{
					baseDenom:  threeAssetRecordBC.Asset1Denom,
					quoteDenom: threeAssetRecordBC.Asset0Denom,
					overrideSp: sdk.OneDec().Add(sdk.OneDec()),
				},
			},

			expectedHistoricalRecords: []expectedResults{
				// The original record AB at t.
				{
					spotPriceA: threeAssetRecordAB.P0LastSpotPrice,
					spotPriceB: threeAssetRecordAB.P1LastSpotPrice,
				},
				// The original record AB at t + 1.
				{
					spotPriceA:    tPlus10sp5ThreeAssetRecordAB.P0LastSpotPrice,
					spotPriceB:    tPlus10sp5ThreeAssetRecordAB.P1LastSpotPrice,
					lastErrorTime: tPlus10sp5ThreeAssetRecordAB.Time,
				},
				// The new record AB added.
				{
					spotPriceA:    sdk.OneDec(),
					spotPriceB:    sdk.OneDec().Add(sdk.OneDec()),
					lastErrorTime: tPlus10sp5ThreeAssetRecordAB.Time,
					isMostRecent:  true,
				},
				// The original record AC at t.
				{
					spotPriceA: threeAssetRecordAC.P0LastSpotPrice,
					spotPriceB: threeAssetRecordAC.P1LastSpotPrice,
				},
				// The original record AC at t + 1.
				{
					spotPriceA: tPlus10sp5ThreeAssetRecordAC.P0LastSpotPrice,
					spotPriceB: tPlus10sp5ThreeAssetRecordAC.P1LastSpotPrice,
				},
				// The new record AC added.
				{
					spotPriceA:    sdk.OneDec(),
					spotPriceB:    types.MaxSpotPrice,                            // Although the price returned from AMM was MaxSpotPrice + 1, it is reset to just MaxSpotPrice.
					lastErrorTime: threeAssetRecordAC.Time.Add(time.Second * 11), // equals to block time
					isMostRecent:  true,
				},
				// The original record BC at t.
				{
					spotPriceA: threeAssetRecordBC.P0LastSpotPrice,
					spotPriceB: threeAssetRecordBC.P1LastSpotPrice,
				},
				// The original record BC at t + 1.
				{
					spotPriceA: tPlus10sp5ThreeAssetRecordBC.P0LastSpotPrice,
					spotPriceB: tPlus10sp5ThreeAssetRecordBC.P1LastSpotPrice,
				},
				// The new record BC added.
				{
					spotPriceA:   sdk.OneDec(),
					spotPriceB:   sdk.OneDec().Add(sdk.OneDec()),
					isMostRecent: true,
				},
			},
		},
	}

	for name, tc := range tests {
		s.Run(name, func() {
			s.SetupTest()
			twapKeeper := s.App.TwapKeeper
			ctx := s.Ctx.WithBlockTime(tc.blockTime)

			if len(tc.spOverrides) > 0 {
				ammMock := twapmock.NewProgrammedAmmInterface(s.App.GAMMKeeper)

				for _, sp := range tc.spOverrides {
					ammMock.ProgramPoolSpotPriceOverride(tc.poolId, sp.baseDenom, sp.quoteDenom, sp.overrideSp, sp.overrideErr)
					ammMock.ProgramPoolDenomsOverride(tc.poolId, []string{sp.baseDenom, sp.quoteDenom}, nil)
				}

				twapKeeper.SetAmmInterface(ammMock)
			}

			s.preSetRecords(tc.preSetRecords)

			err := twapKeeper.UpdateRecords(ctx, tc.poolId)

			if tc.expectError != nil {
				s.Require().ErrorIs(err, tc.expectError)
				return
			}

			s.Require().NoError(err)

			poolMostRecentRecords, err := twapKeeper.GetAllMostRecentRecordsForPool(ctx, tc.poolId)
			s.Require().NoError(err)

			expectedMostRecentRecords := make([]expectedResults, 0)
			for _, historical := range tc.expectedHistoricalRecords {
				if historical.isMostRecent {
					expectedMostRecentRecords = append(expectedMostRecentRecords, historical)
				}
			}

			validateRecords(expectedMostRecentRecords, poolMostRecentRecords)

			poolHistoricalRecords := s.getAllHistoricalRecordsForPool(tc.poolId)
			s.Require().NoError(err)
			validateRecords(tc.expectedHistoricalRecords, poolHistoricalRecords)
		})
	}
}

func (s *TestSuite) TestAfterCreatePool() {
	tests := map[string]struct {
		poolId    uint64
		poolCoins sdk.Coins
		// if this field is set true, we swap in the same block with pool creation
		runSwap     bool
		expectedErr bool
	}{
		"Pool not existing": {
			poolId:      2,
			expectedErr: true,
		},
		"Default Pool, no swap on pool creation block": {
			poolId:    1,
			poolCoins: defaultTwoAssetCoins,
			runSwap:   false,
		},
		"Default Pool, swap on pool creation block": {
			poolId:    1,
			poolCoins: defaultTwoAssetCoins,
			runSwap:   true,
		},
		"Multi assets pool, no swap on pool creation block": {
			poolId:    1,
			poolCoins: defaultThreeAssetCoins,
			runSwap:   false,
		},
		"Multi assets pool, swap on pool creation block": {
			poolId:    1,
			poolCoins: defaultThreeAssetCoins,
			runSwap:   true,
		},
	}

	for name, tc := range tests {
		s.Run(name, func() {
			s.SetupTest()
			var poolId uint64

			// set up pool with input coins
			if tc.poolCoins != nil {
				poolId = s.PrepareBalancerPoolWithCoins(tc.poolCoins...)
				if tc.runSwap {
					s.RunBasicSwap(poolId)
				}
			}

			err := s.twapkeeper.AfterCreatePool(s.Ctx, tc.poolId)
			if tc.expectedErr {
				s.Require().Error(err)
				return
			}
			s.Require().Equal(tc.poolId, poolId)
			s.Require().NoError(err)

			denoms := osmoutils.CoinsDenoms(tc.poolCoins)
			denomPairs := types.GetAllUniqueDenomPairs(denoms)
			expectedRecords := []types.TwapRecord{}
			for _, denomPair := range denomPairs {
				expectedRecord, err := twap.NewTwapRecord(s.App.GAMMKeeper, s.Ctx, poolId, denomPair.Denom0, denomPair.Denom1)
				s.Require().NoError(err)
				expectedRecords = append(expectedRecords, expectedRecord)
			}

			// consistency check that the number of records is exactly equal to the number of denompairs
			allRecords, err := s.twapkeeper.GetAllMostRecentRecordsForPool(s.Ctx, poolId)
			s.Require().NoError(err)
			s.Require().Equal(len(denomPairs), len(allRecords))
			s.Require().Equal(len(expectedRecords), len(allRecords))

			// check on the correctness of all individual twap records
			for i, denomPair := range denomPairs {
				actualRecord, err := s.twapkeeper.GetMostRecentRecordStoreRepresentation(s.Ctx, poolId, denomPair.Denom0, denomPair.Denom1)
				s.Require().NoError(err)
				s.Require().Equal(expectedRecords[i], actualRecord)
				actualRecord, err = s.twapkeeper.GetRecordAtOrBeforeTime(s.Ctx, poolId, s.Ctx.BlockTime(), denomPair.Denom0, denomPair.Denom1)
				s.Require().NoError(err)
				s.Require().Equal(expectedRecords[i], actualRecord)
			}

			// test that after creating a pool
			// has triggered `trackChangedPool`,
			// and that we have the state of price impacted pools.
			changedPools := s.twapkeeper.GetChangedPools(s.Ctx)
			s.Require().Equal(1, len(changedPools))
			s.Require().Equal(tc.poolId, changedPools[0])
		})
	}
}

func (s *TestSuite) TestTwapLog() {
	var expectedErrTolerance = osmomath.MustNewDecFromStr("0.000000000000000100")
	// "Twaplog{912648174127941279170121098210.928219201902041311} = 99.525973560175362367"
	// From: https://www.wolframalpha.com/input?i2d=true&i=log+base+2+of+912648174127941279170121098210.928219201902041311+with+20+digits
	var priceValue = osmomath.MustNewDecFromStr("912648174127941279170121098210.928219201902041311")
	var expectedValue = osmomath.MustNewDecFromStr("99.525973560175362367")

	result := twap.TwapLog(priceValue.SDKDec())
	result_by_customBaseLog := priceValue.CustomBaseLog(osmomath.BigDecFromSDKDec(twap.GeometricTwapMathBase))
	s.Require().True(expectedValue.Sub(osmomath.BigDecFromSDKDec(result)).Abs().LTE(expectedErrTolerance))
	s.Require().True(result_by_customBaseLog.Sub(osmomath.BigDecFromSDKDec(result)).Abs().LTE(expectedErrTolerance))

}

func (s *TestSuite) TestTwapPow() {
	var expectedErrTolerance = osmomath.MustNewDecFromStr("0.00000100")
	// "TwapPow(0.5) = 1.41421356"
	// From: https://www.wolframalpha.com/input?i2d=true&i=power+base+2+exponent+0.5+with+9+digits
	exponentValue := osmomath.MustNewDecFromStr("0.5")
	expectedValue := osmomath.MustNewDecFromStr("1.41421356")

	result := twap.TwapPow(exponentValue.SDKDec())
	result_by_mathPow := math.Pow(twap.GeometricTwapMathBase.MustFloat64(), exponentValue.SDKDec().MustFloat64())
	s.Require().True(expectedValue.Sub(osmomath.BigDecFromSDKDec(result)).Abs().LTE(expectedErrTolerance))
	s.Require().True(osmomath.MustNewDecFromStr(fmt.Sprint(result_by_mathPow)).Sub(osmomath.BigDecFromSDKDec(result)).Abs().LTE(expectedErrTolerance))
}

func testCaseFromDeltas(startAccum, accumDiff sdk.Dec, timeDelta time.Duration, expectedTwap sdk.Dec) computeTwapTestCase {
	return computeTwapTestCase{
		newOneSidedRecord(baseTime, startAccum, true),
		newOneSidedRecord(baseTime.Add(timeDelta), startAccum.Add(accumDiff), true),
		[]twap.TwapType{twap.ArithmeticTwapType},
		denom0,
		expectedTwap,
		false,
		false,
	}
}

func testCaseFromDeltasAsset1(startAccum, accumDiff sdk.Dec, timeDelta time.Duration, expectedTwap sdk.Dec) computeTwapTestCase {
	return computeTwapTestCase{
		newOneSidedRecord(baseTime, startAccum, false),
		newOneSidedRecord(baseTime.Add(timeDelta), startAccum.Add(accumDiff), false),
		[]twap.TwapType{twap.ArithmeticTwapType},
		denom1,
		expectedTwap,
		false,
		false,
	}
}

func geometricTestCaseFromDeltas0(startAccum, accumDiff sdk.Dec, timeDelta time.Duration, expectedTwap sdk.Dec) computeTwapTestCase {
	return computeTwapTestCase{
		newOneSidedGeometricRecord(baseTime, startAccum),
		newOneSidedGeometricRecord(baseTime.Add(timeDelta), startAccum.Add(accumDiff)),
		[]twap.TwapType{twap.GeometricTwapType},
		denom0,
		expectedTwap,
		false,
		false,
	}
}

func geometricTestCaseFromDeltas1(startAccum, accumDiff sdk.Dec, timeDelta time.Duration, expectedTwap sdk.Dec) computeTwapTestCase {
	return geometricTestCaseFromDeltas0(startAccum, accumDiff, timeDelta, sdk.OneDec().Quo(expectedTwap))
}

func testThreeAssetCaseFromDeltas(startAccum, accumDiff sdk.Dec, timeDelta time.Duration, expectedTwap sdk.Dec) computeThreeAssetArithmeticTwapTestCase {
	return computeThreeAssetArithmeticTwapTestCase{
		newThreeAssetOneSidedRecord(baseTime, startAccum, true),
		newThreeAssetOneSidedRecord(baseTime.Add(timeDelta), startAccum.Add(accumDiff), true),
		[]string{denom0, denom0, denom1},
		[]sdk.Dec{expectedTwap, expectedTwap, expectedTwap},
		false,
	}
}
