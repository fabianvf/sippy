package sippyserver

import (
	"fmt"
	"regexp"
	"time"

	sippyprocessingv1 "github.com/openshift/sippy/pkg/apis/sippyprocessing/v1"
	testgridv1 "github.com/openshift/sippy/pkg/apis/testgrid/v1"
	"github.com/openshift/sippy/pkg/buganalysis"
	"github.com/openshift/sippy/pkg/testgridanalysis/testgridanalysisapi"
	"github.com/openshift/sippy/pkg/testgridanalysis/testgridconversion"
	"github.com/openshift/sippy/pkg/testgridanalysis/testgridhelpers"
	"github.com/openshift/sippy/pkg/testgridanalysis/testidentification"
	"github.com/openshift/sippy/pkg/testgridanalysis/testreportconversion"
	"github.com/openshift/sippy/pkg/util/sets"
	"k8s.io/klog"
)

// TestGridLoadingOptions control the data which is loaded from disk into the testgrid structs
type TestGridLoadingConfig struct {
	// LocalData is the directory where the testgrid data is stored
	LocalData string
	// JobFilter is a regex run against job names. Only match names are loaded.
	JobFilter *regexp.Regexp
}

// RawJobResultsAnalysisOptions control which subset of data from the testgrid data is analyzed into the rawJobResults
type RawJobResultsAnalysisConfig struct {
	StartDay int
	NumDays  int
}

// DisplayDataOptions controls how the RawJobResults are processed and prepared for display
type DisplayDataConfig struct {
	MinTestRuns             int
	TestSuccessThreshold    float64
	FailureClusterThreshold int
}

// TestReportGeneratorConfig is a static configuration that can be re-used across multiple invocations of PrepareTestReport with different versions
type TestReportGeneratorConfig struct {
	TestGridLoadingConfig       TestGridLoadingConfig
	RawJobResultsAnalysisConfig RawJobResultsAnalysisConfig
	DisplayDataConfig           DisplayDataConfig
}

// PrepareTestReport is expensive.  It
//  1. gathers test grid data from disk
//  2. proceses that data to produce RawJobResults which look more how humans read testgrid
//  3. uses the RawJobResults to produce a bug cache of relevant bugs
//  4. converts the result of that into a display API object.
func (a *TestReportGeneratorConfig) PrepareTestReport(
	dashboard TestGridDashboardCoordinates,
	syntheticTestManager testgridconversion.SythenticTestManager,
	variantManager testidentification.VariantManager,
	bugCache buganalysis.BugCache,
) sippyprocessingv1.TestReport {
	testGridJobDetails, lastUpdateTime := testgridhelpers.LoadTestGridDataFromDisk(a.TestGridLoadingConfig.LocalData, dashboard.TestGridDashboardNames, a.TestGridLoadingConfig.JobFilter)
	return a.prepareTestReportFromData(dashboard.OpenshiftRelease, syntheticTestManager, variantManager, bugCache, testGridJobDetails, lastUpdateTime)
}

// prepareTestReportFromData should always remain private unless refactored. it's a convenient way to re-use the test grid data deserialized from disk.
func (a *TestReportGeneratorConfig) prepareTestReportFromData(
	openshiftRelease string,
	syntheticTestManager testgridconversion.SythenticTestManager,
	variantManager testidentification.VariantManager,
	bugCache buganalysis.BugCache,
	testGridJobDetails []testgridv1.JobDetails,
	lastUpdateTime time.Time,
) sippyprocessingv1.TestReport {
	rawJobResultOptions := testgridconversion.ProcessingOptions{
		SythenticTestManager: syntheticTestManager,
		StartDay:             a.RawJobResultsAnalysisConfig.StartDay,
		NumDays:              a.RawJobResultsAnalysisConfig.NumDays,
	}
	rawJobResults, processingWarnings := rawJobResultOptions.ProcessTestGridDataIntoRawJobResults(testGridJobDetails)
	bugCacheWarnings := updateBugCacheForJobResults(bugCache, rawJobResults)
	warnings := []string{}
	warnings = append(warnings, processingWarnings...)
	warnings = append(warnings, bugCacheWarnings...)

	return testreportconversion.PrepareTestReport(
		rawJobResults,
		variantManager,
		bugCache,
		openshiftRelease,
		a.DisplayDataConfig.MinTestRuns,
		a.DisplayDataConfig.TestSuccessThreshold,
		a.RawJobResultsAnalysisConfig.NumDays,
		warnings,
		lastUpdateTime,
		a.DisplayDataConfig.FailureClusterThreshold,
	)
}

// PrepareStandardTestReports returns the current period, current two day period, and the previous seven days period
func (a TestReportGeneratorConfig) PrepareStandardTestReports(
	dashboard TestGridDashboardCoordinates,
	syntheticTestManager testgridconversion.SythenticTestManager,
	variantManager testidentification.VariantManager,
	bugCache buganalysis.BugCache,
) StandardReport {
	testGridJobDetails, lastUpdateTime := testgridhelpers.LoadTestGridDataFromDisk(a.TestGridLoadingConfig.LocalData, dashboard.TestGridDashboardNames, a.TestGridLoadingConfig.JobFilter)

	currentPeriodReports := []sippyprocessingv1.TestReport{}
	previousWeekReports := []sippyprocessingv1.TestReport{}

	for i := 1; i <= a.RawJobResultsAnalysisConfig.NumDays; i++ {
		config := a.deepCopy()
		config.RawJobResultsAnalysisConfig.NumDays = i
		configReport := config.prepareTestReportFromData(dashboard.OpenshiftRelease, syntheticTestManager, variantManager, bugCache, testGridJobDetails, lastUpdateTime)
		currentPeriodReports = append(currentPeriodReports, configReport)

		previousSevenDayPeriodConfig := config.deepCopy()
		if config.RawJobResultsAnalysisConfig.StartDay >= 0 {
			previousSevenDayPeriodConfig.RawJobResultsAnalysisConfig.StartDay = config.RawJobResultsAnalysisConfig.StartDay + config.RawJobResultsAnalysisConfig.NumDays
		} else {
			previousSevenDayPeriodConfig.RawJobResultsAnalysisConfig.StartDay = config.RawJobResultsAnalysisConfig.StartDay - config.RawJobResultsAnalysisConfig.NumDays
		}
		previousSevenDayPeriodConfig.RawJobResultsAnalysisConfig.NumDays = 7
		previousWeekReports = append(previousWeekReports, previousSevenDayPeriodConfig.prepareTestReportFromData(dashboard.OpenshiftRelease, syntheticTestManager, variantManager, bugCache, testGridJobDetails, lastUpdateTime))
	}

	return StandardReport{
		CurrentPeriodReports: currentPeriodReports,
		PreviousWeekReports:  previousWeekReports,
	}
}

// updateBugCacheForJobResults looks up all the bugs related to every failing test in the jobResults and returns a list of
// warnings/errors that happened looking up the data
func updateBugCacheForJobResults(bugCache buganalysis.BugCache, rawJobResults testgridanalysisapi.RawData) []string {
	warnings := []string{}

	// now that we have all the test failures (remember we added sythentics), use that to update the bugzilla cache
	failedTestNamesAcrossAllJobRuns := getFailedTestNamesFromJobResults(rawJobResults.JobResults)
	if err := bugCache.UpdateForFailedTests(failedTestNamesAcrossAllJobRuns.List()...); err != nil {
		klog.Error(err)
		warnings = append(warnings, fmt.Sprintf("Bugzilla Lookup Error: an error was encountered looking up existing bugs for failing tests, some test failures may have associated bugs that are not listed below.  Lookup error: %v", err.Error()))
	}
	if err := bugCache.UpdateJobBlockers(sets.StringKeySet(rawJobResults.JobResults).List()...); err != nil {
		klog.Error(err)
		warnings = append(warnings, fmt.Sprintf("Bugzilla Lookup Error: an error was encountered looking up existing bugs for failing tests, some test failures may have associated bugs that are not listed below.  Lookup error: %v", err.Error()))
	}

	return warnings
}

func getFailedTestNamesFromJobResults(jobResults map[string]testgridanalysisapi.RawJobResult) sets.String {
	failedTestNames := sets.NewString()
	for _, jobResult := range jobResults {
		for _, jobrun := range jobResult.JobRunResults {
			failedTestNames.Insert(jobrun.FailedTestNames...)
		}
	}
	return failedTestNames
}

func (a TestReportGeneratorConfig) deepCopy() TestReportGeneratorConfig {
	ret := TestReportGeneratorConfig{
		TestGridLoadingConfig: TestGridLoadingConfig{
			LocalData: a.TestGridLoadingConfig.LocalData,
		},
		RawJobResultsAnalysisConfig: RawJobResultsAnalysisConfig{
			StartDay: a.RawJobResultsAnalysisConfig.StartDay,
			NumDays:  a.RawJobResultsAnalysisConfig.NumDays,
		},
		DisplayDataConfig: DisplayDataConfig{
			MinTestRuns:             a.DisplayDataConfig.MinTestRuns,
			TestSuccessThreshold:    a.DisplayDataConfig.TestSuccessThreshold,
			FailureClusterThreshold: a.DisplayDataConfig.FailureClusterThreshold,
		},
	}
	if a.TestGridLoadingConfig.JobFilter != nil {
		ret.TestGridLoadingConfig.JobFilter = a.TestGridLoadingConfig.JobFilter.Copy()
	}

	return ret
}
