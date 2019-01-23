package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/sts"
	docker "github.com/fsouza/go-dockerclient"
	"github.com/sirupsen/logrus"
	flag "github.com/spf13/pflag"
)

const (
	exitCodeOk             int = 0
	exitCodeError          int = 1
	exitCodeDockerError    int = 2
	exitCodeFlagParseError     = 10 + iota
	exitCodeAWSError
)

var (
	Version = "v0.1.7"
)

var (
	f = flag.NewFlagSet("flags", flag.ExitOnError)

	// options
	helpFlag    = f.BoolP("help", "h", false, "help")
	logFlag     = f.BoolP("logs", "l", false, "logs")
	mountFlags  = f.StringSliceP("mount", "m", []string{}, "src:dest")
	envFlags    = f.StringSliceP("env", "e", []string{}, "key=value")
	profileFlag = f.StringP("profile", "p", "", "AWS profile")
	regionFlag  = f.StringP("region", "r", "", "AWS region")
	verboseFlag = f.BoolP("verbose", "v", false, "verbose")
)

const helpString = `Usage:
  ecs-local [-hv] [-l] [--profile=aws_profile] [--region=aws_region] [-e key=value] [-m src:dest] [task_def] [command...]

Flags:
  -h, --help    Print this help message
  -l, --logs	Mount pry and irb local log volumes
  -m, --mount   Mount src file or directory into container (-m $(pwd)/test:/srv/testtest)
  -e, --env     Set environments variable
  -p, --profile The AWS profile to use
  -r, --region  The AWS region the table is in
  -v, --verbose Verbose logging`

var log = logrus.New()

func help() {
	fmt.Printf("ecs-local %s\n%s\n", Version, helpString)
	os.Exit(exitCodeOk)
}

func main() {
	f.Usage = help
	f.Parse(os.Args[1:])
	args := f.Args()

	if *helpFlag == true {
		help()
	}

	log.SetLevel(logrus.ErrorLevel)
	if *verboseFlag == true {
		log.SetLevel(logrus.DebugLevel)
	}

	if len(args) < 1 {
		help()
	}

	taskDefinitionName := args[0]

	// set desired AWS region
	awsRegion := "us-east-1"
	if envRegion, present := os.LookupEnv("AWS_REGION"); present {
		awsRegion = envRegion
	}
	if *regionFlag != "" {
		awsRegion = *regionFlag
	}

	// set desired AWS profile
	awsProfile := "default"
	if envProfile, present := os.LookupEnv("AWS_PROFILE"); present {
		awsProfile = envProfile
	}
	if *profileFlag != "" {
		awsProfile = *profileFlag
	}
	log.Debugf("Using AWS region \"%s\" ", awsRegion)
	log.Debugf("Using AWS profile \"%s\" ", awsProfile)

	// override default sts session duration
	stscreds.DefaultDuration = time.Duration(1) * time.Hour

	sess := session.Must(session.NewSessionWithOptions(session.Options{
		AssumeRoleTokenProvider: stscreds.StdinTokenProvider,
		SharedConfigState:       session.SharedConfigEnable,
		Profile:                 awsProfile,
		Config:                  aws.Config{Region: aws.String(awsRegion)},
	}))

	sess.Config.Credentials = credentials.NewCredentials(&CredentialCacheProvider{
		Creds:   sess.Config.Credentials,
		Profile: awsProfile,
	})

	svc := ecs.New(sess)
	resp, err := svc.DescribeTaskDefinition(&ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(taskDefinitionName),
	})

	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(exitCodeAWSError)
	}

	if log.Level == logrus.DebugLevel {
		creds, _ := sess.Config.Credentials.Get()
		log.Debugf("Credential provider is %s", creds.ProviderName)
	}

	task := resp.TaskDefinition
	image := task.ContainerDefinitions[0].Image

	log.Debugf("Found task %s", *task.TaskDefinitionArn)

	ecrClient := ecr.New(sess)
	input := &ecr.GetAuthorizationTokenInput{}
	result, err := ecrClient.GetAuthorizationToken(input)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(exitCodeDockerError)
	}

	authData := result.AuthorizationData[0]
	token := authData.AuthorizationToken

	data, err := base64.StdEncoding.DecodeString(*token)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(exitCodeDockerError)
	}

	userpass := strings.Split(string(data), ":")
	if len(userpass) != 2 {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(exitCodeDockerError)
	}

	auth := docker.AuthConfiguration{
		Username:      userpass[0],
		Password:      userpass[1],
		ServerAddress: *authData.ProxyEndpoint,
	}

	endpoint := "unix:///var/run/docker.sock"
	client, err := docker.NewClient(endpoint)

	pullOptions := docker.PullImageOptions{
		Repository: *image,
	}

	fmt.Printf("Pulling %s \n", *image)
	pullOptions.OutputStream = os.Stdout

	err = client.PullImage(pullOptions, auth)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(exitCodeDockerError)
	}

	dockerArgs := []string{"run", "-it", "--rm"}

	// set docker command
	command := args[1:]
	if len(command) == 0 {
		for _, v := range task.ContainerDefinitions[0].Command {
			command = append(command, *v)
		}
	}
	log.Debugf("Running command \"%s\"", strings.Join(command, " "))

	// envs
	for _, e := range task.ContainerDefinitions[0].Environment {
		dockerArgs = append(dockerArgs, "-e", fmt.Sprintf("%s=%s", *e.Name, *e.Value))
	}

	// attempt to assume container IAM role
	if task.TaskRoleArn != nil {
		stsClient := sts.New(sess)
		role, err := stsClient.AssumeRole(&sts.AssumeRoleInput{
			DurationSeconds: aws.Int64(3600),
			RoleArn:         task.TaskRoleArn,
			RoleSessionName: aws.String("ecs-local"),
		})
		if err != nil {
			log.Debugf("Unable to assume role %s", *task.TaskRoleArn)
			log.Debugf("%s", err.Error())
		} else {
			log.Debugf("Successfully assumed container role %s", *task.TaskRoleArn)
			dockerArgs = append(dockerArgs,
				"-e", fmt.Sprintf("AWS_ACCESS_KEY_ID=%s", *role.Credentials.AccessKeyId),
				"-e", fmt.Sprintf("AWS_SECRET_ACCESS_KEY=%s", *role.Credentials.SecretAccessKey),
				"-e", fmt.Sprintf("AWS_SESSION_TOKEN=%s", *role.Credentials.SessionToken),
			)
		}
	}
	if *logFlag == true {
		// get the current working dir
		dir, err := os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
		// touch the history files if they don't already exist in the current working dir
		os.OpenFile(fmt.Sprintf("%s/.pry_history", dir), os.O_RDONLY|os.O_CREATE, 0666)
		os.OpenFile(fmt.Sprintf("%s/.irb_history", dir), os.O_RDONLY|os.O_CREATE, 0666)
		os.OpenFile(fmt.Sprintf("%s/.irb-history", dir), os.O_RDONLY|os.O_CREATE, 0666)
		// append the file mounts to the docker command args
		dockerArgs = append(dockerArgs,
			"-v", fmt.Sprintf("%s/.pry_history:/srv/.pry_history", dir),
			"-v", fmt.Sprintf("%s/.irb-history:/srv/.irb-history", dir),
			"-v", fmt.Sprintf("%s/.irb_history:/srv/.irb_history", dir),
		)
	}

	// parse mount flags
	if len(*mountFlags) > 0 {
		for _, mount := range *mountFlags {
			parts := strings.SplitN(mount, ":", 2)
			dockerArgs = append(dockerArgs,
				"-v", fmt.Sprintf("%s:%s", parts[0], parts[1]))
		}
	}

	// parse environment flags
	if len(*envFlags) > 0 {
		for _, env := range *envFlags {
			parts := strings.SplitN(env, "=", 2)
			dockerArgs = append(dockerArgs,
				"-e", fmt.Sprintf("%s=%s", parts[0], parts[1]))
		}
	}

	dockerArgs = append(dockerArgs, *image)

	// start the container
	cmd := exec.Command("docker", append(dockerArgs, command...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	cmd.Start()
	cmd.Wait()
}
