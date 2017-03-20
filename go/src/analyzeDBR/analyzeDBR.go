package main

/*
Imports of interest:
  "github.com/BurntSushi/toml" - TOML parser
  "flag" inbuilt package for commmand line parameter parsing
  "github.com/mohae/deepcopy" package to copy any arbitary struct / map etc
  "Numerous AWS packages" offical AWS SDK's
*/
import (
  "log"
  "github.com/BurntSushi/toml"
  "flag"
  "io/ioutil"
  "os"
  "errors"
  "regexp"
  "encoding/json"
  "net/http"
  "bytes"
  "strings"
  "time"
  "strconv"
  "github.com/aws/aws-sdk-go/aws"
  "github.com/aws/aws-sdk-go/aws/session"
  "github.com/aws/aws-sdk-go/service/cloudwatch"
  "github.com/aws/aws-sdk-go/service/ec2"
  "github.com/mohae/deepcopy"
)
/*
Structs Below are used to contain configuration parsed in
*/
type General struct {
  Namespace string
}

type RI struct {
  Enabled bool `toml:"enableRIanalysis"`
  TotalUtilization bool `toml:"enableRITotalUtilization"`
  PercentThreshold int `toml:"riPercentageThreshold"`
  TotalThreshold int `toml:"riTotalThreshold"`
  CwName  string
  CwNameTotal string
  CwDimension string
  CwDimensionTotal string
  CwType string
  Sql string
  Ignore map[string]int
}

type Athena struct {
  DbSQL string `toml:"create_database"`
  TableSQL string `toml:"create_table"`
  TableBlendedSQL string `toml:"create_table_blended"`
  Test string
}

type Metric struct {
  Enabled bool
  Type string
  SQL string
  CwName string
  CwDimension string
  CwType string
}

type Config struct {
  General General
  RI RI
  Athena Athena
  Metrics []Metric
}
/*
End of configuraton structs
*/

/*
Structs for Athena requests / responses. Requests go through a proxy on the same server
Proxy accepts and gives back JSON.
*/
type AthenaRequest struct {
  AthenaUrl string `json:"athenaUrl"`
  S3StagingDir string `json:"s3StagingDir"`
  AwsSecretKey string `json:"awsSecretKey"`
  AwsAccessKey string `json:"awsAccessKey"`
  Query string `json:"query"`
}

type AthenaResponse struct {
  Columns []map[string]string
  Rows []map[string]string
}

var defaultConfigPath = "./analyzeDBR.config"

/*
Function reads in and validates command line parameters
*/
func getParams(configFile *string, account *string, region *string, key *string, secret *string, date *string, bucket *string, blended *bool) error {

  // Define input command line config parameter and parse it
  flag.StringVar(configFile, "config", defaultConfigPath, "Input config file for analyzeDBR")
  flag.StringVar(key, "key", "", "Athena IAM access key")
  flag.StringVar(secret, "secret", "", "Athena IAM secret key")
  flag.StringVar(region, "region", "", "Athena Region")
  flag.StringVar(account, "account", "", "AWS Account #")
  flag.StringVar(date, "date", "", "Current month in YYYY-MM format")
  flag.StringVar(bucket, "bucket", "", "AWS Bucket where DBR files sit")
  flag.BoolVar(blended, "blended", false, "Set to 1 if DBR file contains blended costs")

  flag.Parse()

  // check input against defined regex's
  r_empty 	:= regexp.MustCompile(`^$`)
  r_region 	:= regexp.MustCompile(`^\w+-\w+-\d$`)
  r_account	:= regexp.MustCompile(`^\d+$`)
  r_date	  := regexp.MustCompile(`^\d{6}$`)

  if r_empty.MatchString(*key) {
  return errors.New("Must provide Athena access key")
  }
  if r_empty.MatchString(*secret) {
  return errors.New("Must provide Athena secret key")
  }
  if r_empty.MatchString(*bucket) {
  return errors.New("Must provide valid AWS DBR bucket")
  }
  if ! r_region.MatchString(*region) {
  return errors.New("Must provide valid AWS region")
  }
  if ! r_account.MatchString(*account) {
  return errors.New("Must provide valid AWS account number")
  }
  if ! r_date.MatchString(*date) {
  return errors.New("Must provide valid date (YYYY-MM)")
  }

  return nil
}

/*
Function reads in configuration file provided in configFile input
Config file is stored in TOML format
*/
func getConfig(conf *Config, configFile string) error {

    // check for existance of file
    if _, err := os.Stat(configFile); err != nil {
      return errors.New("Config File " + configFile + " does not exist")
    }

    // read file
    b, err := ioutil.ReadFile(configFile)
  if err != nil {
      return err
    }

    // parse TOML config file into struct
    if _, err := toml.Decode(string(b), &conf); err != nil {
      return err
    }

    return nil
}

/*
Function substitutes parameters into SQL command.
Input map contains key (thing to look for in input sql) and if found replaces with given value)
*/
func substituteParams(sql string, params map[string]string) string {

  for sub, value := range params {
  sql = strings.Replace(sql, sub, value, -1)
  }

  return sql
}

/*
Function takes SQL to send to Athena converts into JSON to send to Athena HTTP proxy and then sends it.
Then recieves responses in JSON which is converted back into a struct and returned
*/
func sendQuery(key string, secret string, region string, account string, sql string) (AthenaResponse, error) {

  // construct json
  req := AthenaRequest {
  AwsAccessKey: key,
  AwsSecretKey: secret,
  AthenaUrl: "jdbc:awsathena://athena." + region + ".amazonaws.com:443",
  S3StagingDir: "s3://aws-athena-query-results-" + account + "-" + region + "/",
  Query: sql }

  // encode into JSON
  b := new(bytes.Buffer)
  err := json.NewEncoder(b).Encode(req)
  if err != nil {
  return AthenaResponse{}, err
  }

  // send request through proxy
  resp, err := http.Post("http://127.0.0.1:10000/query", "application/json", b)
  if err != nil {
  return AthenaResponse{}, err
  }

  // check status code
  if resp.StatusCode != 200 {
  respBytes, _ := ioutil.ReadAll(resp.Body)
  return AthenaResponse{}, errors.New(string(respBytes))
  }

  // decode json into response struct
  var results AthenaResponse
  err = json.NewDecoder(resp.Body).Decode(&results)
  if err != nil {
  respBytes, _ := ioutil.ReadAll(resp.Body)
  return AthenaResponse{}, errors.New(string(respBytes))
  }

  return results, nil
}

/*
Function takes metric data (from Athena etal) and sends through to cloudwatch.
*/
func sendMetric(svc *cloudwatch.CloudWatch, data AthenaResponse, cwNameSpace string, cwName string, cwType string, cwDimensionName string) error {

  input := cloudwatch.PutMetricDataInput{}
  input.Namespace = aws.String(cwNameSpace)
  i := 0
  for row := range data.Rows {
    // skip metric if dimension or value is empty
    if len(data.Rows[row]["dimension"]) < 1 || len(data.Rows[row]["value"]) < 1 {
      continue
    }

  // send Metric Data as we have reached 20 records, and clear MetricData Array
  if i >= 20 {
  _, err := svc.PutMetricData(&input)
  if err != nil {
  return err
  }
  input.MetricData = nil
  i = 0
  }

  t, _ := time.Parse("2006-01-02 15", data.Rows[row]["date"])
  v, _ := strconv.ParseFloat(data.Rows[row]["value"], 64)
  metric := cloudwatch.MetricDatum{
            MetricName: aws.String(cwName),
            Timestamp:  aws.Time(t),
            Unit:       aws.String(cwType),
  Value:			aws.Float64(v),
    }

    // Dimension can be a single or comma seperated list of values, or key/values
    // presence of "=" sign in value designates key=value. Otherwise input cwDimensionName is used as key
    d := strings.Split(data.Rows[row]["dimension"], ",")
    for i := range d  {
      var dname, dvalue string
      if strings.Contains(d[i], "=") {
        dTuple := strings.Split(d[i], "=")
        dname = dTuple[0]
        dvalue = dTuple[1]
      } else {
        dname = cwDimensionName
        dvalue = d[i]
      }
      cwD := cloudwatch.Dimension{
            Name: aws.String(dname),
            Value: aws.String(dvalue),
          }
      metric.Dimensions = append(metric.Dimensions, &cwD)
    }

  input.MetricData = append(input.MetricData, &metric)
  i++
  }

  // if we still have data to send - send it
  if len(input.MetricData) > 0 {
    _, err := svc.PutMetricData(&input)
    if err != nil {
        return err
    }
  }

  return nil
}

/*
Function processes a single hours worth of RI usage and compares against available RIs to produce % utiization / under-utilization
*/
func riUtilizationHour(svc *cloudwatch.CloudWatch, date string, used map[string]map[string]map[string]int, azRI map[string]map[string]map[string]int, regionRI map[string]map[string]int, conf Config, region string) error {

  // Perform Deep Copy of both RI maps.
  // We need a copy of the maps as we decrement the RI's available by the hourly usage and a map is a pointer
  // hence decrementing the original maps will affect the pass-by-reference data
  cpy := deepcopy.Copy(azRI)
  t_azRI, ok := cpy.(map[string]map[string]map[string]int)
  if ! ok {
    return errors.New("could not copy AZ RI map")
  }

  cpy = deepcopy.Copy(regionRI)
  t_regionRI, ok := cpy.(map[string]map[string]int)
  if ! ok {
    return errors.New("could not copy Regional RI map")
  }

  // Iterate through used hours decrementing any available RI's per hour's that were used
  // AZ specific RI's are first checked and then regional RI's
  for az := range used {
    for instance := range used[az] {
      // check if azRI for this region even exist
      _, ok := t_azRI[az][instance]
      if ok {
        for platform := range used[az][instance] {
          // check if azRI for this region and platform even exists
          _, ok2 := t_azRI[az][instance][platform]
            if ok2 {
              // More RI's than we used
              if t_azRI[az][instance][platform] >= used[az][instance][platform] {
                t_azRI[az][instance][platform] -= used[az][instance][platform]
                used[az][instance][platform] = 0
              } else {
                // Less RI's than we used
                used[az][instance][platform] -= t_azRI[az][instance][platform]
                t_azRI[az][instance][platform] = 0
              }
            }
          }
        }

      // check if regionRI even exists and that instance used is in the right region
      _, ok = t_regionRI[instance]
      if ok && az[:len(az)-1] == region {
        for platform := range used[az][instance] {
          // if we still have more used instances check against regional RI's
          if used[az][instance][platform] > 0 && t_regionRI[instance][platform] > 0 {
            if t_regionRI[instance][platform] >= used[az][instance][platform] {
              t_regionRI[instance][platform] -= used[az][instance][platform]
              used[az][instance][platform] = 0
            } else {
              used[az][instance][platform] -= t_regionRI[instance][platform]
              t_regionRI[instance][platform] = 0
            }
          }
        }
      }
    }
  }

  // Now loop through the temp RI data to check if any RI's are still available
  // If they are and the % of un-use is above the configured threshold then colate for sending to cloudwatch
  // We sum up the total of regional and AZ specific RI's so that we get one instance based metric regardless of region or AZ RI
  i_unused := make(map[string]map[string]int)
  i_total  := make(map[string]map[string]int)
  var unused int
  var total int

  for az := range t_azRI {
    for instance := range t_azRI[az] {
      _, ok := i_unused[instance]
      if ! ok {
        i_unused[instance] = make(map[string]int)
        i_total[instance] = make(map[string]int)
      }
      for platform := range t_azRI[az][instance] {
        i_total[instance][platform] = azRI[az][instance][platform]
        i_unused[instance][platform] = t_azRI[az][instance][platform]
        total += azRI[az][instance][platform]
        unused += t_azRI[az][instance][platform]
      }
    }
  }

  for instance := range t_regionRI {
    for platform := range t_regionRI[instance] {
      _, ok := i_unused[instance]
      if ! ok {
        i_unused[instance] = make(map[string]int)
        i_total[instance] = make(map[string]int)
      }
      i_total[instance][platform] += regionRI[instance][platform]
      i_unused[instance][platform] += t_regionRI[instance][platform]
      total += regionRI[instance][platform]
      unused += t_regionRI[instance][platform]
    }
  }

  // loop over per-instance utilization and build metrics to send
  metrics := AthenaResponse{}
  for instance := range i_unused {
    _, ok := conf.RI.Ignore[instance]
    if !ok { // instance not on ignore list
      for platform := range i_unused[instance] {
        percent := (float64(i_unused[instance][platform]) / float64(i_total[instance][platform])) * 100
        if int(percent) > conf.RI.PercentThreshold && i_total[instance][platform] > conf.RI.TotalThreshold {
          metrics.Rows = append(metrics.Rows, map[string]string{"dimension": "instance=" + instance + ",platform=" + platform, "date": date, "value": strconv.FormatInt(int64(percent), 10)})
        }
      }
    }
  }


  // send per instance type under-utilization
  if len(metrics.Rows) > 0 {
    if err := sendMetric(svc, metrics, conf.General.Namespace, conf.RI.CwName, conf.RI.CwType, conf.RI.CwDimension); err != nil {
      log.Fatal(err)
    }
  }

  // If confured send overall total utilization
  if conf.RI.TotalUtilization {
    percent := 100 - ((float64(unused) / float64(total)) * 100)
    total := AthenaResponse{}
    total.Rows = append(total.Rows, map[string]string{"dimension": "hourly", "date": date, "value": strconv.FormatInt(int64(percent), 10)})
    if err := sendMetric(svc, total, conf.General.Namespace, conf.RI.CwNameTotal, conf.RI.CwType, conf.RI.CwDimensionTotal); err != nil {
      log.Fatal(err)
    }
  }

  return nil
}

/*
Main RI function. Gest RI and usage data (from Athena).
Then loops through every hour and calls riUtilizationHour to process each hours worth of data
*/
func riUtilization(sess *session.Session, conf Config, key string, secret string, region string, account string, date string) error {

  svc := ec2.New(sess)

  params := &ec2.DescribeReservedInstancesInput{
    DryRun: aws.Bool(false),
    Filters: []*ec2.Filter{
       {
           Name: aws.String("state"),
           Values: []*string{
               aws.String("active"),
           },
       },
   },
 }

  resp, err := svc.DescribeReservedInstances(params)
  if err != nil {
    return err
  }

  az_ri := make(map[string]map[string]map[string]int)
  region_ri := make(map[string]map[string]int)

  // map in number of RI's available both AZ specific and regional
  for i := range resp.ReservedInstances {
    ri := resp.ReservedInstances[i]

    // Trim VPC identifier of Platform type as its not relevant for RI Utilization calculations
    platform := strings.TrimSuffix(*ri.ProductDescription, " (Amazon VPC)")

    if *ri.Scope == "Availability Zone" {
      _, ok := az_ri[*ri.AvailabilityZone]
      if ! ok {
        az_ri[*ri.AvailabilityZone] = make(map[string]map[string]int)
      }
      _, ok = az_ri[*ri.AvailabilityZone][*ri.InstanceType]
      if ! ok {
        az_ri[*ri.AvailabilityZone][*ri.InstanceType] = make(map[string]int)
      }
      az_ri[*ri.AvailabilityZone][*ri.InstanceType][platform] += int(*ri.InstanceCount)
    } else if  *ri.Scope == "Region" {
      _, ok := region_ri[*ri.InstanceType]
      if ! ok {
        region_ri[*ri.InstanceType] = make(map[string]int)
      }
      region_ri[*ri.InstanceType][platform] += int(*ri.InstanceCount)
    }
  }

  // Fetch RI hours used
  data, err := sendQuery(key, secret, region, account, substituteParams(conf.RI.Sql, map[string]string{"**DATE**": date }))
  if err != nil {
    log.Fatal(err)
  }

  // loop through response data and generate map of hourly usage, per AZ, per instance, per platform
  hours := make(map[string]map[string]map[string]map[string]int)
  for row := range data.Rows {
    _, ok := hours[data.Rows[row]["date"]]
    if ! ok {
      hours[data.Rows[row]["date"]] = make(map[string]map[string]map[string]int)
    }
    _, ok = hours[data.Rows[row]["date"]][data.Rows[row]["az"]]
    if ! ok {
      hours[data.Rows[row]["date"]][data.Rows[row]["az"]] = make(map[string]map[string]int)
    }
    _, ok = hours[data.Rows[row]["date"]][data.Rows[row]["az"]][data.Rows[row]["instance"]]
    if ! ok {
      hours[data.Rows[row]["date"]][data.Rows[row]["az"]][data.Rows[row]["instance"]] = make(map[string]int)
    }

    v, _ := strconv.ParseInt(data.Rows[row]["hours"], 10, 64)
    hours[data.Rows[row]["date"]][data.Rows[row]["az"]][data.Rows[row]["instance"]][data.Rows[row]["platform"]] += int(v)
  }

  // Create new cloudwatch client.
  svcCloudwatch := cloudwatch.New(sess)

  // Iterate through each hour and compare the number of instances used vs the number of RIs available
  // If RI leftover percentage is > 1% push to cloudwatch
  for hour := range hours {
    if err := riUtilizationHour(svcCloudwatch, hour, hours[hour], az_ri, region_ri, conf, region); err != nil {
      return err
    }
  }
  return nil
}

func main() {

  var configFile, region, key, secret, account, bucket, date, costColumn string
  var blendedDBR bool
  if err := getParams(&configFile, &account, &region, &key, &secret, &date, &bucket, &blendedDBR); err != nil {
  log.Fatal(err)
  }

  var conf Config
  if err := getConfig(&conf, configFile); err != nil {
    log.Fatal(err)
  }

  // make sure Athena DB exists - dont care about results
  if _, err := sendQuery(key, secret, region, account, conf.Athena.DbSQL); err != nil {
  log.Fatal(err)
  }

  // make sure current Athena table exists - dont care about results
  // Depending on the Type of DBR (blended or not blended - the table we create is slightly different)
  if blendedDBR {
  costColumn = "blendedcost"
  if _, err := sendQuery(key, secret, region, account, substituteParams(conf.Athena.TableBlendedSQL, map[string]string{"**BUCKET**": bucket, "**DATE**": date, "**ACCOUNT**": account})); err != nil {
  log.Fatal(err)
  }
  } else {
  costColumn = "cost"
  if _, err := sendQuery(key, secret, region, account, substituteParams(conf.Athena.TableSQL, map[string]string{"**BUCKET**": bucket, "**DATE**": date, "**ACCOUNT**": account})); err != nil {
  log.Fatal(err)
  }
  }

  /// initialize AWS GO client
  sess, err := session.NewSession(&aws.Config{Region: aws.String(region)})
  if err != nil {
  log.Fatal(err)
  }

  // Create new cloudwatch client.
  svc := cloudwatch.New(sess)

  // If RI analysis enabled - do it
  if conf.RI.Enabled {
    if err := riUtilization(sess, conf, key, secret, region, account, date); err != nil {
      log.Fatal(err)
    }
  }

  // iterate through metrics - perform query then send data to cloudwatch
  for metric := range conf.Metrics {
  if ! conf.Metrics[metric].Enabled {
  continue
  }
  results, err := sendQuery(key, secret, region, account, substituteParams(conf.Metrics[metric].SQL, map[string]string{"**DATE**": date, "**COST**": costColumn}))
  if err != nil {
  log.Fatal(err)
  }
  if conf.Metrics[metric].Type == "dimension-per-row" {
  if err := sendMetric(svc, results, conf.General.Namespace, conf.Metrics[metric].CwName, conf.Metrics[metric].CwType, conf.Metrics[metric].CwDimension); err != nil {
  log.Fatal(err)
  }
  }
  }
}
