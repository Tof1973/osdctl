package cluster

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "github.com/openshift-online/ocm-sdk-go/servicelogs/v1"

	"github.com/openshift/osdctl/cmd/servicelog"

	pd "github.com/PagerDuty/go-pagerduty"
	"github.com/andygrunwald/go-jira"
	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	"github.com/aws/aws-sdk-go-v2/service/cloudtrail/types"
	cmv1 "github.com/openshift-online/ocm-sdk-go/clustersmgmt/v1"
	"github.com/openshift/osdctl/cmd/cluster/dynatrace"
	"github.com/openshift/osdctl/pkg/osdCloud"
	"github.com/openshift/osdctl/pkg/osdctlConfig"
	"github.com/openshift/osdctl/pkg/printer"
	"github.com/openshift/osdctl/pkg/provider/pagerduty"
	"github.com/openshift/osdctl/pkg/utils"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

const (
	JiraBaseURL                   = "https://issues.redhat.com"
	JiraTokenRegistrationPath     = "/secure/ViewProfile.jspa?selectedTab=com.atlassian.pats.pats-plugin:jira-user-personal-access-tokens"
	PagerDutyTokenRegistrationUrl = "https://martindstone.github.io/PDOAuth/"
	ClassicSplunkURL              = "https://osdsecuritylogs.splunkcloud.com/en-US/app/search/search?q=search%%20index%%3D%%22%s%%22%%20clusterid%%3D%%22%s%%22\n\n"
	HCPSplunkURL                  = "https://osdsecuritylogs.splunkcloud.com/en-US/app/search/search?q=search%%20index%%3D%%22%s%%22%%20annotations.managed.openshift.io%%2Fhosted-cluster-id%%3Docm-%s-%s-%s\n\n"
	shortOutputConfigValue        = "short"
	longOutputConfigValue         = "long"
	jsonOutputConfigValue         = "json"
	delimiter                     = ">> "
)

type contextOptions struct {
	cluster *cmv1.Cluster

	output            string
	verbose           bool
	full              bool
	clusterID         string
	externalClusterID string
	baseDomain        string
	organizationID    string
	days              int
	pages             int
	oauthtoken        string
	usertoken         string
	infraID           string
	awsProfile        string
	jiratoken         string
	team_ids          []string
}

type contextData struct {
	// Cluster info
	ClusterName    string
	ClusterVersion string
	ClusterID      string

	// Current OCM environment (e.g., "production" or "stage")
	OCMEnv string

	// Dynatrace Environment URL
	DyntraceEnvURL string

	// limited Support Status
	LimitedSupportReasons []*cmv1.LimitedSupportReason
	// Service Logs
	ServiceLogs []*v1.LogEntry

	// Jira Cards
	JiraIssues        []jira.Issue
	SupportExceptions []jira.Issue

	// PD Alerts
	pdServiceID      []string
	PdAlerts         map[string][]pd.Incident
	HistoricalAlerts map[string][]*pagerduty.IncidentOccurrenceTracker

	// CloudTrail Logs
	CloudtrailEvents []*types.Event

	// OCM Cluster description
	Description string
}

// newCmdContext implements the context command to show the current context of a cluster
func newCmdContext() *cobra.Command {
	ops := newContextOptions()
	contextCmd := &cobra.Command{
		Use:               "context",
		Short:             "Shows the context of a specified cluster",
		Args:              cobra.ExactArgs(1),
		DisableAutoGenTag: true,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(ops.complete(cmd, args))
			cmdutil.CheckErr(ops.run())
		},
	}

	contextCmd.Flags().StringVarP(&ops.output, "output", "o", "long", "Valid formats are ['long', 'short', 'json']. Output is set to 'long' by default")
	contextCmd.Flags().StringVarP(&ops.clusterID, "cluster-id", "C", "", "Cluster ID")
	contextCmd.Flags().StringVarP(&ops.awsProfile, "profile", "p", "", "AWS Profile")
	contextCmd.Flags().BoolVarP(&ops.verbose, "verbose", "", false, "Verbose output")
	contextCmd.Flags().BoolVar(&ops.full, "full", false, "Run full suite of checks.")
	contextCmd.Flags().IntVarP(&ops.days, "days", "d", 30, "Command will display X days of Error SLs sent to the cluster. Days is set to 30 by default")
	contextCmd.Flags().IntVar(&ops.pages, "pages", 40, "Command will display X pages of Cloud Trail logs for the cluster. Pages is set to 40 by default")
	contextCmd.Flags().StringVar(&ops.oauthtoken, "oauthtoken", "", fmt.Sprintf("Pass in PD oauthtoken directly. If not passed in, by default will read `pd_oauth_token` from ~/.config/%s.\nPD OAuth tokens can be generated by visiting %s", osdctlConfig.ConfigFileName, PagerDutyTokenRegistrationUrl))
	contextCmd.Flags().StringVar(&ops.usertoken, "usertoken", "", fmt.Sprintf("Pass in PD usertoken directly. If not passed in, by default will read `pd_user_token` from ~/config/%s", osdctlConfig.ConfigFileName))
	contextCmd.Flags().StringVar(&ops.jiratoken, "jiratoken", "", fmt.Sprintf("Pass in the Jira access token directly. If not passed in, by default will read `jira_token` from ~/.config/%s.\nJira access tokens can be registered by visiting %s/%s", osdctlConfig.ConfigFileName, JiraBaseURL, JiraTokenRegistrationPath))
	contextCmd.Flags().StringArrayVarP(&ops.team_ids, "team-ids", "t", []string{}, fmt.Sprintf("Pass in PD team IDs directly to filter the PD Alerts by team. Can also be defined as `team_ids` in ~/.config/%s\nWill show all PD Alerts for all PD service IDs if none is defined", osdctlConfig.ConfigFileName))
	return contextCmd
}

func newContextOptions() *contextOptions {
	return &contextOptions{}
}

func (o *contextOptions) complete(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return cmdutil.UsageErrorf(cmd, "Provide exactly one cluster ID")
	}

	if o.days < 1 {
		return fmt.Errorf("cannot have a days value lower than 1")
	}

	// Create OCM client to talk to cluster API
	defer utils.StartDelayTracker(o.verbose, "OCM Clusters").End()
	ocmClient, err := utils.CreateConnection()
	if err != nil {
		return err
	}
	defer func() {
		if err := ocmClient.Close(); err != nil {
			fmt.Printf("Cannot close the ocmClient (possible memory leak): %q", err)
		}
	}()

	clusters := utils.GetClusters(ocmClient, args)
	if len(clusters) != 1 {
		return fmt.Errorf("unexpected number of clusters matched input. Expected 1 got %d", len(clusters))
	}

	o.cluster = clusters[0]
	o.clusterID = o.cluster.ID()
	o.externalClusterID = o.cluster.ExternalID()
	o.baseDomain = o.cluster.DNS().BaseDomain()
	o.infraID = o.cluster.InfraID()

	if o.usertoken == "" {
		o.usertoken = viper.GetString(pagerduty.PagerDutyUserTokenConfigKey)
	}

	if o.oauthtoken == "" {
		o.oauthtoken = viper.GetString(pagerduty.PagerDutyOauthTokenConfigKey)
	}

	orgID, err := utils.GetOrgfromClusterID(ocmClient, *o.cluster)
	if err != nil {
		fmt.Printf("Failed to get Org ID for cluster ID %s - err: %q", o.clusterID, err)
		o.organizationID = ""
	} else {
		o.organizationID = orgID
	}

	return nil
}

func (o *contextOptions) run() error {
	var printFunc func(*contextData)
	switch o.output {
	case shortOutputConfigValue:
		printFunc = o.printShortOutput
	case longOutputConfigValue:
		printFunc = o.printLongOutput
	case jsonOutputConfigValue:
		printFunc = o.printJsonOutput
	default:
		return fmt.Errorf("unknown Output Format: %s", o.output)
	}

	currentData, dataErrors := o.generateContextData()
	if currentData == nil {
		fmt.Fprintf(os.Stderr, "Failed to query cluster info: %+v", dataErrors)
		os.Exit(1)
	}

	if len(dataErrors) > 0 {
		fmt.Fprintf(os.Stderr, "Encountered Errors during data collection. Displayed data may be incomplete: \n")
		for _, dataError := range dataErrors {
			fmt.Fprintf(os.Stderr, "\t%v\n", dataError)
		}
	}

	printFunc(currentData)

	return nil
}

func (o *contextOptions) printLongOutput(data *contextData) {
	data.printClusterHeader()

	fmt.Println(strings.TrimSpace(data.Description))
	fmt.Println()
	utils.PrintLimitedSupportReasons(data.LimitedSupportReasons)
	fmt.Println()
	printJIRASupportExceptions(data.SupportExceptions)
	fmt.Println()
	utils.PrintServiceLogs(data.ServiceLogs, o.verbose, o.days)
	fmt.Println()
	utils.PrintJiraIssues(data.JiraIssues)
	fmt.Println()
	utils.PrintPDAlerts(data.PdAlerts, data.pdServiceID)
	fmt.Println()

	if o.full {
		printHistoricalPDAlertSummary(data.HistoricalAlerts, data.pdServiceID, o.days)
		fmt.Println()

		printCloudTrailLogs(data.CloudtrailEvents)
		fmt.Println()
	}

	// Print other helpful links
	o.printOtherLinks(data)
	fmt.Println()

	// Print Dynatrace URL
	printDynatraceEnvURL(data)
}

func (o *contextOptions) printShortOutput(data *contextData) {
	data.printClusterHeader()

	highAlertCount := 0
	lowAlertCount := 0
	for _, alerts := range data.PdAlerts {
		for _, alert := range alerts {
			if strings.ToLower(alert.Urgency) == "high" {
				highAlertCount++
			} else {
				lowAlertCount++
			}
		}
	}

	historicalAlertsString := "N/A"
	historicalAlertsCount := 0
	if data.HistoricalAlerts != nil {
		for _, histAlerts := range data.HistoricalAlerts {
			for _, histAlert := range histAlerts {
				historicalAlertsCount += histAlert.Count
			}
		}
		historicalAlertsString = fmt.Sprintf("%d", historicalAlertsCount)
	}

	var numInternalServiceLogs int
	for _, serviceLog := range data.ServiceLogs {
		if serviceLog.InternalOnly() {
			numInternalServiceLogs++
		}
	}

	table := printer.NewTablePrinter(os.Stdout, 20, 1, 2, ' ')
	table.AddRow([]string{
		"Version",
		"Supported?",
		fmt.Sprintf("SLs (last %d d)", o.days),
		"Jira Tickets",
		"Current Alerts",
		fmt.Sprintf("Historical Alerts (last %d d)", o.days),
	})
	table.AddRow([]string{
		data.ClusterVersion,
		fmt.Sprintf("%t", len(data.LimitedSupportReasons) == 0),
		fmt.Sprintf("%d (%d internal)", len(data.ServiceLogs), numInternalServiceLogs),
		fmt.Sprintf("%d", len(data.JiraIssues)),
		fmt.Sprintf("H: %d | L: %d", highAlertCount, lowAlertCount),
		historicalAlertsString,
	})

	if err := table.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "Error printing Short Output: %v\n", err)
	}
}

func (o *contextOptions) printJsonOutput(data *contextData) {
	jsonOut, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Can't marshal results to json: %v\n", err)
		return
	}

	fmt.Println(string(jsonOut))
}

// generateContextData Creates a contextData struct that contains all the
// cluster context information requested by the contextOptions. if a certain
// data point can not be queried, the appropriate field will be null and the
// errors array will contain information about the error. The first return
// value will only be nil, if this function fails to get basic cluster
// information. The second return value will *never* be nil, but instead have a
// length of 0 if no errors occurred
func (o *contextOptions) generateContextData() (*contextData, []error) {
	data := &contextData{}
	errors := []error{}

	wg := sync.WaitGroup{}

	// For PD query dependencies
	pdwg := sync.WaitGroup{}
	var skipPagerDutyCollection bool
	pdProvider, err := pagerduty.NewClient().
		WithUserToken(o.usertoken).
		WithOauthToken(o.oauthtoken).
		WithBaseDomain(o.baseDomain).
		WithTeamIdList(viper.GetStringSlice(pagerduty.PagerDutyTeamIDsKey)).
		Init()
	if err != nil {
		skipPagerDutyCollection = true
		errors = append(errors, fmt.Errorf("skipping PagerDuty context collection: %v", err))
	}

	ocmClient, err := utils.CreateConnection()
	if err != nil {
		return nil, []error{err}
	}
	defer ocmClient.Close()
	// Normally the o.cluster would be set by complete function, but in case we want to call this function
	// in an other context, we can make sure o.cluster is set properly from o.clusterID
	if o.cluster == nil {
		cluster, err := utils.GetCluster(ocmClient, o.clusterID)
		if err != nil {
			errors = append(errors, err)
			return nil, errors
		}
		o.cluster = cluster
	}

	data.ClusterName = o.cluster.Name()
	data.ClusterID = o.clusterID
	data.ClusterVersion = o.cluster.Version().RawID()
	data.OCMEnv = utils.GetCurrentOCMEnv(ocmClient)

	GetLimitedSupport := func() {
		defer wg.Done()
		defer utils.StartDelayTracker(o.verbose, "Limited Support reasons").End()
		limitedSupportReasons, err := utils.GetClusterLimitedSupportReasons(ocmClient, o.clusterID)
		if err != nil {
			errors = append(errors, fmt.Errorf("error while getting Limited Support status reasons: %v", err))
		} else {
			data.LimitedSupportReasons = append(data.LimitedSupportReasons, limitedSupportReasons...)
		}
	}

	GetServiceLogs := func() {
		defer wg.Done()
		defer utils.StartDelayTracker(o.verbose, "Service Logs").End()
		timeToCheckSvcLogs := time.Now().AddDate(0, 0, -o.days)
		data.ServiceLogs, err = servicelog.GetServiceLogsSince(o.clusterID, timeToCheckSvcLogs, false, false)
		if err != nil {
			errors = append(errors, fmt.Errorf("error while getting the service logs: %v", err))
		}
	}

	GetJiraIssues := func() {
		defer wg.Done()
		defer utils.StartDelayTracker(o.verbose, "Jira Issues").End()
		data.JiraIssues, err = utils.GetJiraIssuesForCluster(o.clusterID, o.externalClusterID)
		if err != nil {
			errors = append(errors, fmt.Errorf("error while getting the open jira tickets: %v", err))
		}
	}

	GetSupportExceptions := func() {
		defer wg.Done()
		defer utils.StartDelayTracker(o.verbose, "Support Exceptions").End()
		data.SupportExceptions, err = utils.GetJiraSupportExceptionsForOrg(o.organizationID)
		if err != nil {
			errors = append(errors, fmt.Errorf("error while getting support exceptions: %v", err))
		}
	}

	GetDynatraceURL := func() {
		var clusterID string = o.clusterID
		defer wg.Done()
		defer utils.StartDelayTracker(o.verbose, "Dynatrace URL").End()

		clusterID, _, err := dynatrace.GetManagementCluster(ocmClient, o.cluster)
		if err != nil {
			errors = append(errors, err)
			data.DyntraceEnvURL = err.Error()
			return
		}
		data.DyntraceEnvURL, err = dynatrace.GetDynatraceURLFromLabel(ocmClient, clusterID)
		if err != nil {
			errors = append(errors, fmt.Errorf("error The Dynatrace Environemnt URL could not be determined from Label. Using fallback method%s", err))
			// FallBack method to determine via Cluster Login
			data.DyntraceEnvURL, err = dynatrace.GetDynatraceURLFromManagementCluster(clusterID)
			if err != nil {
				errors = append(errors, fmt.Errorf("error The Dynatrace Environemnt URL could not be determined %s", err))
				data.DyntraceEnvURL = "the Dynatrace Environemnt URL could not be determined. \nPlease refer the SOP to determine the correct Dyntrace Tenant URL- https://github.com/openshift/ops-sop/tree/master/dynatrace#what-environments-are-there"
			}
		}
	}

	GetPagerDutyAlerts := func() {
		pdwg.Add(1)
		defer wg.Done()
		defer pdwg.Done()

		if skipPagerDutyCollection {
			return
		}

		delayTracker := utils.StartDelayTracker(o.verbose, "PagerDuty Service")
		data.pdServiceID, err = pdProvider.GetPDServiceIDs()
		if err != nil {
			errors = append(errors, fmt.Errorf("error getting PD Service ID: %v", err))
		}
		delayTracker.End()

		defer utils.StartDelayTracker(o.verbose, "current PagerDuty Alerts").End()
		data.PdAlerts, err = pdProvider.GetFiringAlertsForCluster(data.pdServiceID)
		if err != nil {
			errors = append(errors, fmt.Errorf("error while getting current PD Alerts: %v", err))
		}
	}

	var retrievers []func()

	retrievers = append(
		retrievers,
		GetLimitedSupport,
		GetServiceLogs,
		GetJiraIssues,
		GetSupportExceptions,
		GetPagerDutyAlerts,
		GetDynatraceURL,
	)

	if o.output == longOutputConfigValue {

		GetDescription := func() {
			defer wg.Done()
			defer utils.StartDelayTracker(o.verbose, "Cluster Description").End()

			cmd := "ocm describe cluster " + o.clusterID
			output, err := exec.Command("bash", "-c", cmd).Output()
			if err != nil {
				fmt.Fprintln(os.Stderr, string(output))
				fmt.Fprintln(os.Stderr, err)
			}
			data.Description = string(output)
		}

		retrievers = append(
			retrievers,
			GetDescription,
		)
	}

	if o.full {
		GetHistoricalPagerDutyAlerts := func() {
			pdwg.Wait()
			defer wg.Done()
			defer utils.StartDelayTracker(o.verbose, "historical PagerDuty Alerts").End()
			data.HistoricalAlerts, err = pdProvider.GetHistoricalAlertsForCluster(data.pdServiceID)
			if err != nil {
				errors = append(errors, fmt.Errorf("error while getting historical PD Alert Data: %v", err))
			}
		}

		GetCloudTrailLogs := func() {
			defer wg.Done()
			defer utils.StartDelayTracker(o.verbose, fmt.Sprintf("past %d pages of Cloudtrail data", o.pages)).End()
			data.CloudtrailEvents, err = GetCloudTrailLogsForCluster(o.awsProfile, o.clusterID, o.pages)
			if err != nil {
				errors = append(errors, fmt.Errorf("error getting cloudtrail logs for cluster: %v", err))
			}
		}

		retrievers = append(
			retrievers,
			GetHistoricalPagerDutyAlerts,
			GetCloudTrailLogs,
		)
	}

	for _, retriever := range retrievers {
		wg.Add(1)
		go retriever()
	}

	wg.Wait()

	return data, errors
}

func GetCloudTrailLogsForCluster(awsProfile string, clusterID string, maxPages int) ([]*types.Event, error) {
	awsJumpClient, err := osdCloud.GenerateAWSClientForCluster(awsProfile, clusterID)
	if err != nil {
		return nil, err
	}

	var foundEvents []types.Event

	eventSearchInput := cloudtrail.LookupEventsInput{}

	for counter := 0; counter <= maxPages; counter++ {
		print(".")
		cloudTrailEvents, err := awsJumpClient.LookupEvents(&eventSearchInput)
		if err != nil {
			return nil, err
		}

		foundEvents = append(foundEvents, cloudTrailEvents.Events...)

		// for pagination
		eventSearchInput.NextToken = cloudTrailEvents.NextToken
		if cloudTrailEvents.NextToken == nil {
			break
		}
	}
	var filteredEvents []*types.Event
	for _, event := range foundEvents {
		if skippableEvent(*event.EventName) {
			continue
		}
		if event.Username != nil && strings.Contains(*event.Username, "RH-SRE-") {
			continue
		}
		filteredEvents = append(filteredEvents, &event)
	}

	return filteredEvents, nil
}

func printHistoricalPDAlertSummary(incidentCounters map[string][]*pagerduty.IncidentOccurrenceTracker, serviceIDs []string, sinceDays int) {
	var name string = "PagerDuty Historical Alerts"
	fmt.Println(delimiter + name)

	for _, serviceID := range serviceIDs {

		if len(incidentCounters[serviceID]) == 0 {
			fmt.Println("Service: https://redhat.pagerduty.com/service-directory/" + serviceID + ": None")
			continue
		}

		fmt.Println("Service: https://redhat.pagerduty.com/service-directory/" + serviceID + ":")
		table := printer.NewTablePrinter(os.Stdout, 20, 1, 3, ' ')
		table.AddRow([]string{"Type", "Count", "Last Occurrence"})
		totalIncidents := 0
		for _, incident := range incidentCounters[serviceID] {
			table.AddRow([]string{incident.IncidentName, strconv.Itoa(incident.Count), incident.LastOccurrence})
			totalIncidents += incident.Count
		}

		// Add empty row for readability
		table.AddRow([]string{})
		if err := table.Flush(); err != nil {
			fmt.Fprintf(os.Stderr, "Error printing %s: %v\n", name, err)
		}

		fmt.Println("\tTotal number of incidents [", totalIncidents, "] in [", sinceDays, "] days")
	}
}

func printJIRASupportExceptions(issues []jira.Issue) {
	var name string = "Support Exceptions"
	fmt.Println(delimiter + name)

	for _, i := range issues {
		fmt.Printf("[%s](%s/%s): %+v [Status: %s]\n", i.Key, i.Fields.Type.Name, i.Fields.Priority.Name, i.Fields.Summary, i.Fields.Status.Name)
		fmt.Printf("- Link: %s/browse/%s\n\n", JiraBaseURL, i.Key)
	}

	if len(issues) == 0 {
		fmt.Println("None")
	}
}

func (o *contextOptions) printOtherLinks(data *contextData) {
	var name string = "External resources"
	fmt.Println(delimiter + name)

	links := map[string]string{
		"OHSS Cards":        fmt.Sprintf("%s/issues/?jql=project%%20%%3D%%20OHSS%%20and%%20(%%22Cluster%%20ID%%22%%20~%%20%%20%%22%s%%22%%20OR%%20%%22Cluster%%20ID%%22%%20~%%20%%22%s%%22)", JiraBaseURL, o.clusterID, o.externalClusterID),
		"CCX dashboard":     fmt.Sprintf("https://kraken.psi.redhat.com/clusters/%s", o.externalClusterID),
		"Splunk Audit Logs": o.buildSplunkURL(data),
	}

	if data.pdServiceID != nil {
		for _, id := range data.pdServiceID {
			links[fmt.Sprintf("PagerDuty Service %s", id)] = fmt.Sprintf("https://redhat.pagerduty.com/service-directory/%s", id)
		}
	}

	// Sort, so it's always a predictable order
	var keys []string
	for k := range links {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	table := printer.NewTablePrinter(os.Stdout, 20, 1, 3, ' ')
	for _, link := range keys {
		table.AddRow([]string{link, strings.TrimSpace(links[link])})
	}

	if err := table.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "Error printing %s: %v\n", name, err)
	}
}

func (o *contextOptions) buildSplunkURL(data *contextData) string {
	// Determine the relevant Splunk URL
	if o.cluster.Hypershift().Enabled() {
		switch data.OCMEnv {
		case "production":
			return fmt.Sprintf(HCPSplunkURL, "openshift_managed_hypershift_audit", "production", o.cluster.ID(), o.cluster.Name())
		case "stage":
			return fmt.Sprintf(HCPSplunkURL, "openshift_managed_hypershift_audit_stage", "staging", o.cluster.ID(), o.cluster.Name())
		default:
			return ""
		}
	} else {
		switch data.OCMEnv {
		case "production":
			return fmt.Sprintf(ClassicSplunkURL, "openshift_managed_audit", o.infraID)
		case "stage":
			return fmt.Sprintf(ClassicSplunkURL, "openshift_managed_audit_stage", o.infraID)
		default:
			return ""
		}
	}
}

func printCloudTrailLogs(events []*types.Event) {
	var name string = "Potentially interesting CloudTrail events"
	fmt.Println(delimiter + name)

	if events == nil {
		fmt.Println("None")
		return
	}

	table := printer.NewTablePrinter(os.Stdout, 20, 1, 3, ' ')
	table.AddRow([]string{"EventId", "EventName", "Username", "EventTime"})
	for _, event := range events {
		if event.Username == nil {
			table.AddRow([]string{*event.EventId, *event.EventName, "", event.EventTime.String()})
		} else {
			table.AddRow([]string{*event.EventId, *event.EventName, *event.Username, event.EventTime.String()})
		}
	}
	// Add empty row for readability
	table.AddRow([]string{})
	if err := table.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "Error printing %s: %v\n", name, err)
	}
}

// These are a list of skippable aws event types, as they won't indicate any modification on the customer's side.
func skippableEvent(eventName string) bool {
	skippableList := []string{
		"Get",
		"List",
		"Describe",
		"AssumeRole",
		"Encrypt",
		"Decrypt",
		"LookupEvents",
		"GenerateDataKey",
	}

	for _, skipWord := range skippableList {
		if strings.Contains(eventName, skipWord) {
			return true
		}
	}
	return false
}

func printDynatraceEnvURL(data *contextData) {
	var name string = "Dynatrace Environment URL"
	fmt.Println(delimiter + name)
	fmt.Println(data.DyntraceEnvURL)
}

func (data *contextData) printClusterHeader() {
	clusterHeader := fmt.Sprintf("%s -- %s", data.ClusterName, data.ClusterID)
	fmt.Println(strings.Repeat("=", len(clusterHeader)))
	fmt.Println(clusterHeader)
	fmt.Println(strings.Repeat("=", len(clusterHeader)))
}
