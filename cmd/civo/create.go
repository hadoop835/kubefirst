package civo

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"golang.org/x/term"
	v1 "k8s.io/api/core/v1"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/kubefirst/kubefirst/configs"
	"github.com/kubefirst/kubefirst/internal/argocd"
	"github.com/kubefirst/kubefirst/internal/civo"
	"github.com/kubefirst/kubefirst/internal/downloadManager"
	"github.com/kubefirst/kubefirst/internal/gitClient"
	"github.com/kubefirst/kubefirst/internal/githubWrapper"
	gitlab "github.com/kubefirst/kubefirst/internal/gitlabcloud"
	"github.com/kubefirst/kubefirst/internal/handlers"
	"github.com/kubefirst/kubefirst/internal/helm"
	"github.com/kubefirst/kubefirst/internal/k8s"
	"github.com/kubefirst/kubefirst/internal/progressPrinter"
	"github.com/kubefirst/kubefirst/internal/reports"
	"github.com/kubefirst/kubefirst/internal/services"
	internalssh "github.com/kubefirst/kubefirst/internal/ssh"
	"github.com/kubefirst/kubefirst/internal/ssl"
	"github.com/kubefirst/kubefirst/internal/terraform"
	"github.com/kubefirst/kubefirst/internal/vault"
	"github.com/kubefirst/kubefirst/internal/wrappers"
	"github.com/kubefirst/kubefirst/pkg"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func createCivo(cmd *cobra.Command, args []string) error {

	progressPrinter.AddTracker("preflight-checks", "Running preflight checks", 6)
	progressPrinter.AddTracker("platform-create", "Creating your kubefirst platform", 11)
	progressPrinter.SetupProgress(progressPrinter.TotalOfTrackers(), false)

	alertsEmailFlag, err := cmd.Flags().GetString("alerts-email")
	if err != nil {
		return err
	}

	cloudRegionFlag, err := cmd.Flags().GetString("cloud-region")
	if err != nil {
		return err
	}

	clusterNameFlag, err := cmd.Flags().GetString("cluster-name")
	if err != nil {
		return err
	}

	clusterTypeFlag, err := cmd.Flags().GetString("cluster-type")
	if err != nil {
		return err
	}

	domainNameFlag, err := cmd.Flags().GetString("domain-name")
	if err != nil {
		return err
	}

	dryRunFlag, err := cmd.Flags().GetBool("dry-run")
	if err != nil {
		return err
	}

	githubOwnerFlag, err := cmd.Flags().GetString("github-owner")
	if err != nil {
		return err
	}

	gitlabOwnerFlag, err := cmd.Flags().GetString("gitlab-owner")
	if err != nil {
		return err
	}

	gitProviderFlag, err := cmd.Flags().GetString("git-provider")
	if err != nil {
		return err
	}

	gitopsTemplateURLFlag, err := cmd.Flags().GetString("gitops-template-url")
	if err != nil {
		return err
	}

	gitopsTemplateBranchFlag, err := cmd.Flags().GetString("gitops-template-branch")
	if err != nil {
		return err
	}

	kbotPasswordFlag, err := cmd.Flags().GetString("kbot-password")
	if err != nil {
		return err
	}

	metaphorTemplateURLFlag, err := cmd.Flags().GetString("metaphor-template-url")
	if err != nil {
		return err
	}

	metaphorTemplateBranchFlag, err := cmd.Flags().GetString("metaphor-template-branch")
	if err != nil {
		return err
	}

	useTelemetryFlag, err := cmd.Flags().GetBool("use-telemetry")
	if err != nil {
		return err
	}

	// required for destroy command
	viper.Set("flags.alerts-email", alertsEmailFlag)
	viper.Set("flags.cluster-name", clusterNameFlag)
	viper.Set("flags.domain-name", domainNameFlag)
	viper.Set("flags.dry-run", dryRunFlag)
	viper.Set("flags.git-provider", gitProviderFlag)
	viper.WriteConfig()

	// Set git handlers
	switch gitProviderFlag {
	case "github":
		viper.Set("flags.github-owner", githubOwnerFlag)
	case "gitlab":
		viper.Set("flags.gitlab-owner", gitlabOwnerFlag)
	}

	// Switch based on git provider, set params
	var cGitHost, cGitOwner, cGitToken, cGitUser, containerRegistryHost string
	var cGitlabOwnerGroupID int
	switch gitProviderFlag {
	case "github":
		cGitHost = civo.GithubHost
		cGitOwner = githubOwnerFlag
		cGitToken = os.Getenv("GITHUB_TOKEN")
		containerRegistryHost = "ghcr.io"
	case "gitlab":
		cGitHost = civo.GitlabHost
		cGitOwner = gitlabOwnerFlag
		cGitToken = os.Getenv("GITLAB_TOKEN")
		cGitUser = githubOwnerFlag
		containerRegistryHost = "registry.gitlab.com"
	default:
		log.Error().Msgf("invalid git provider option")
	}

	// Instantiate config
	config := civo.GetConfig(clusterNameFlag, domainNameFlag, gitProviderFlag, cGitOwner)
	// These need to be set for reference elsewhere
	viper.Set(fmt.Sprintf("%s.repos.gitops.git-url", config.GitProvider), config.DestinationGitopsRepoGitURL)
	viper.WriteConfig()

	var sshPrivateKey, sshPublicKey string

	// todo placed in configmap in kubefirst namespace, included in telemetry
	clusterId := viper.GetString("kubefirst.cluster-id")
	if clusterId == "" {
		clusterId = pkg.GenerateClusterID()
		viper.Set("kubefirst.cluster-id", clusterId)
		viper.WriteConfig()
	}
	kubefirstStateStoreBucketName := fmt.Sprintf("k1-state-store-%s-%s", clusterNameFlag, clusterId)

	// Detokenize
	isKubefirstTeam := os.Getenv("KUBEFIRST_TEAM")
	if isKubefirstTeam == "" {
		isKubefirstTeam = "false"
	}

	gitopsDirectoryTokens := civo.GitOpsDirectoryValues{
		AlertsEmail:                    alertsEmailFlag,
		AtlantisAllowList:              fmt.Sprintf("%s/%s/*", cGitHost, cGitOwner),
		CloudProvider:                  civo.CloudProvider,
		CloudRegion:                    cloudRegionFlag,
		ClusterName:                    clusterNameFlag,
		ClusterType:                    clusterTypeFlag,
		DomainName:                     domainNameFlag,
		KubeconfigPath:                 config.Kubeconfig,
		KubefirstStateStoreBucket:      kubefirstStateStoreBucketName,
		KubefirstTeam:                  isKubefirstTeam,
		KubefirstVersion:               configs.K1Version,
		ArgoCDIngressURL:               fmt.Sprintf("https://argocd.%s", domainNameFlag),
		ArgoCDIngressNoHTTPSURL:        fmt.Sprintf("argocd.%s", domainNameFlag),
		ArgoWorkflowsIngressURL:        fmt.Sprintf("https://argo.%s", domainNameFlag),
		ArgoWorkflowsIngressNoHTTPSURL: fmt.Sprintf("argo.%s", domainNameFlag),
		AtlantisIngressURL:             fmt.Sprintf("https://atlantis.%s", domainNameFlag),
		AtlantisIngressNoHTTPSURL:      fmt.Sprintf("atlantis.%s", domainNameFlag),
		ChartMuseumIngressURL:          fmt.Sprintf("https://chartmuseum.%s", domainNameFlag),
		VaultIngressURL:                fmt.Sprintf("https://vault.%s", domainNameFlag),
		VaultIngressNoHTTPSURL:         fmt.Sprintf("vault.%s", domainNameFlag),
		VouchIngressURL:                fmt.Sprintf("https://vouch.%s", domainNameFlag),

		GitDescription:       fmt.Sprintf("%s hosted git", config.GitProvider),
		GitNamespace:         "N/A",
		GitProvider:          config.GitProvider,
		GitRunner:            fmt.Sprintf("%s Runner", config.GitProvider),
		GitRunnerDescription: fmt.Sprintf("Self Hosted %s Runner", config.GitProvider),
		GitRunnerNS:          fmt.Sprintf("%s-runner", config.GitProvider),
		GitURL:               gitopsTemplateURLFlag,

		GitHubHost:  fmt.Sprintf("https://github.com/%s/gitops.git", cGitOwner),
		GitHubOwner: cGitOwner,

		GitlabHost:         civo.GitlabHost,
		GitlabOwner:        cGitOwner,
		GitlabOwnerGroupID: cGitlabOwnerGroupID,

		GitOpsRepoAtlantisWebhookURL: fmt.Sprintf("https://atlantis.%s/events", domainNameFlag),
		GitOpsRepoGitURL:             config.DestinationGitopsRepoGitURL,
		GitOpsRepoNoHTTPSURL:         fmt.Sprintf("%s.com/%s/gitops.git", cGitHost, cGitOwner),
		ClusterId:                    clusterId,
	}

	if useTelemetryFlag {
		gitopsDirectoryTokens.UseTelemetry = "true"
	} else {
		gitopsDirectoryTokens.UseTelemetry = "false"
	}

	viper.Set(fmt.Sprintf("%s.atlantis.webhook.url", config.GitProvider), fmt.Sprintf("https://atlantis.%s/events", domainNameFlag))
	viper.WriteConfig()

	if useTelemetryFlag {
		if err := wrappers.SendSegmentIoTelemetry(domainNameFlag, pkg.MetricInitStarted, civo.CloudProvider, config.GitProvider, clusterId); err != nil {
			log.Info().Msg(err.Error())
			return err
		}
	}

	// this branch flag value is overridden with a tag when running from a
	// kubefirst binary for version compatibility
	if gitopsTemplateBranchFlag == "main" && configs.K1Version != "development" {
		gitopsTemplateBranchFlag = configs.K1Version
	}
	log.Info().Msgf("kubefirst version configs.K1Version: %s ", configs.K1Version)
	log.Info().Msgf("cloning gitops-template repo url: %s ", gitopsTemplateURLFlag)
	log.Info().Msgf("cloning gitops-template repo branch: %s ", gitopsTemplateBranchFlag)
	// this branch flag value is overridden with a tag when running from a
	// kubefirst binary for version compatibility
	if metaphorTemplateBranchFlag == "main" && configs.K1Version != "development" {
		metaphorTemplateBranchFlag = configs.K1Version
	}

	log.Info().Msgf("cloning metaphor template url: %s ", metaphorTemplateURLFlag)
	log.Info().Msgf("cloning metaphor template branch: %s ", metaphorTemplateBranchFlag)

	atlantisWebhookSecret := viper.GetString("secrets.atlantis-webhook")
	if atlantisWebhookSecret == "" {
		atlantisWebhookSecret = pkg.Random(20)
		viper.Set("secrets.atlantis-webhook", atlantisWebhookSecret)
		viper.WriteConfig()
	}

	log.Info().Msg("checking authentication to required providers")

	executionControl := viper.GetBool("kubefirst-checks.cloud-credentials")
	if !executionControl {

		if os.Getenv("CIVO_TOKEN") == "" {
			fmt.Println("\n\nYour CIVO_TOKEN environment variable isn't set,\nvisit this link https://dashboard.civo.com/security to retrieve your token\nand enter it here, then press Enter:")
			civoToken, err := term.ReadPassword(0)
			if err != nil {
				return errors.New("error reading password input from user")
			}

			os.Setenv("CIVO_TOKEN", string(civoToken))
			log.Info().Msg("CIVO_TOKEN set - continuing")
		}
		viper.Set("kubefirst-checks.cloud-credentials", true)
		viper.WriteConfig()
		progressPrinter.IncrementTracker("preflight-checks", 1)
	} else {
		log.Info().Msg("already completed cloud credentials check - continuing")
		progressPrinter.IncrementTracker("preflight-checks", 1)
	}

	executionControl = viper.GetBool("kubefirst-checks.state-store-creds")
	if !executionControl {
		creds, err := civo.GetAccessCredentials(kubefirstStateStoreBucketName, cloudRegionFlag)
		if err != nil {
			log.Info().Msg(err.Error())
		}
		viper.Set("kubefirst.state-store-creds.access-key-id", creds.AccessKeyID)
		viper.Set("kubefirst.state-store-creds.secret-access-key-id", creds.SecretAccessKeyID)
		viper.Set("kubefirst.state-store-creds.name", creds.Name)
		viper.Set("kubefirst.state-store-creds.id", creds.ID)
		viper.Set("kubefirst-checks.state-store-creds", true)
		viper.WriteConfig()
		log.Info().Msg("civo object storage credentials created and set")
		progressPrinter.IncrementTracker("preflight-checks", 1)
	} else {
		log.Info().Msg("already created civo object storage credentials - continuing")
		progressPrinter.IncrementTracker("preflight-checks", 1)
	}

	skipDomainCheck := viper.GetBool("kubefirst-checks.domain-liveness")
	if !skipDomainCheck {
		// domain id
		domainId, err := civo.GetDNSInfo(domainNameFlag, cloudRegionFlag)
		if err != nil {
			log.Info().Msg(err.Error())
		}

		// viper values set in above function
		log.Info().Msgf("domainId: %s", domainId)
		domainLiveness := civo.TestDomainLiveness(false, domainNameFlag, domainId, cloudRegionFlag)
		if !domainLiveness {
			msg := "failed to check the liveness of the Domain. A valid public Domain on the same CIVO " +
				"account as the one where Kubefirst will be installed is required for this operation to " +
				"complete.\nTroubleshoot Steps:\n\n - Make sure you are using the correct CIVO account and " +
				"region.\n - Verify that you have the necessary permissions to access the domain.\n - Check " +
				"that the domain is correctly configured and is a public domain\n - Check if the " +
				"domain exists and has the correct name and domain.\n - If you don't have a Domain," +
				"please follow these instructions to create one: " +
				"https://www.civo.com/learn/configure-dns \n\n" +
				"if you are still facing issues please reach out to support team for further assistance"

			return errors.New(msg)
		}
		viper.Set("kubefirst-checks.domain-liveness", true)
		viper.WriteConfig()
		progressPrinter.IncrementTracker("preflight-checks", 1)
	} else {
		log.Info().Msg("domain check already complete - continuing")
		progressPrinter.IncrementTracker("preflight-checks", 1)
	}

	executionControl = viper.GetBool("kubefirst-checks.state-store-create")
	if !executionControl {
		accessKeyId := viper.GetString("kubefirst.state-store-creds.access-key-id")
		log.Info().Msgf("access key id %s", accessKeyId)

		bucket, err := civo.CreateStorageBucket(accessKeyId, kubefirstStateStoreBucketName, cloudRegionFlag)
		if err != nil {
			log.Info().Msg(err.Error())
			return err
		}

		viper.Set("kubefirst.state-store.id", bucket.ID)
		viper.Set("kubefirst.state-store.name", bucket.Name)
		viper.Set("kubefirst-checks.state-store-create", true)
		viper.WriteConfig()
		log.Info().Msg("civo state store bucket created")
		progressPrinter.IncrementTracker("preflight-checks", 1)
	} else {
		log.Info().Msg("already created civo state store bucket - continuing")
		progressPrinter.IncrementTracker("preflight-checks", 1)
	}

	// Check quotas
	quotaMessage, quotaFailures, quotaWarnings, err := returnCivoQuotaEvaluation(cloudRegionFlag, false)
	if err != nil {
		return err
	}
	switch {
	case quotaFailures > 0:
		fmt.Println(reports.StyleMessage(quotaMessage))
		return errors.New("At least one of your Civo quotas is close to its limit. Please check the error message above for additional details.")
	case quotaWarnings > 0:
		fmt.Println(reports.StyleMessage(quotaMessage))
	}
	//* CIVO END

	executionControl = viper.GetBool(fmt.Sprintf("kubefirst-checks.%s-credentials", config.GitProvider))
	if !executionControl {
		if len(cGitToken) == 0 {
			return errors.New(
				fmt.Sprintf(
					"please set a %s_TOKEN environment variable to continue\n https://docs.kubefirst.io/kubefirst/github/install.html#step-3-kubefirst-init",
					strings.ToUpper(config.GitProvider),
				),
			)
		}

		// Objects to check for
		newRepositoryNames := []string{"gitops", "metaphor-frontend"}
		newTeamNames := []string{"admins", "developers"}

		switch config.GitProvider {
		case "github":
			httpClient := http.DefaultClient
			gitHubService := services.NewGitHubService(httpClient)
			gitHubHandler := handlers.NewGitHubHandler(gitHubService)

			// get github data to set user based on the provided token
			log.Info().Msg("verifying github authentication")
			githubUser, err := gitHubHandler.GetGitHubUser(cGitToken)
			if err != nil {
				return err
			}

			// Set in config and in viper
			gitopsDirectoryTokens.GitHubUser = githubUser
			viper.Set("github.user", githubUser)
			cGitUser = githubUser

			err = viper.WriteConfig()
			if err != nil {
				return err
			}

			err = gitHubHandler.CheckGithubOrganizationPermissions(cGitToken, githubOwnerFlag, githubUser)
			if err != nil {
				return err
			}

			githubWrapper := githubWrapper.New()
			newRepositoryExists := false
			// todo hoist to globals
			errorMsg := "the following repositories must be removed before continuing with your kubefirst installation.\n\t"

			for _, repositoryName := range newRepositoryNames {
				responseStatusCode := githubWrapper.CheckRepoExists(githubOwnerFlag, repositoryName)

				// https://docs.github.com/en/rest/repos/repos?apiVersion=2022-11-28#get-a-repository
				repositoryExistsStatusCode := 200
				repositoryDoesNotExistStatusCode := 404

				if responseStatusCode == repositoryExistsStatusCode {
					log.Info().Msgf("repository https://github.com/%s/%s exists", githubOwnerFlag, repositoryName)
					errorMsg = errorMsg + fmt.Sprintf("https://github.com/%s/%s\n\t", githubOwnerFlag, repositoryName)
					newRepositoryExists = true
				} else if responseStatusCode == repositoryDoesNotExistStatusCode {
					log.Info().Msgf("repository https://github.com/%s/%s does not exist, continuing", githubOwnerFlag, repositoryName)
				}
			}
			if newRepositoryExists {
				return errors.New(errorMsg)
			}

			newTeamExists := false
			errorMsg = "the following teams must be removed before continuing with your kubefirst installation.\n\t"

			for _, teamName := range newTeamNames {
				responseStatusCode := githubWrapper.CheckTeamExists(githubOwnerFlag, teamName)

				// https://docs.github.com/en/rest/teams/teams?apiVersion=2022-11-28#get-a-team-by-name
				teamExistsStatusCode := 200
				teamDoesNotExistStatusCode := 404

				if responseStatusCode == teamExistsStatusCode {
					log.Info().Msgf("team https://github.com/%s/%s exists", githubOwnerFlag, teamName)
					errorMsg = errorMsg + fmt.Sprintf("https://github.com/orgs/%s/teams/%s\n\t", githubOwnerFlag, teamName)
					newTeamExists = true
				} else if responseStatusCode == teamDoesNotExistStatusCode {
					log.Info().Msgf("https://github.com/orgs/%s/teams/%s does not exist, continuing", githubOwnerFlag, teamName)
				}
			}
			if newTeamExists {
				return errors.New(errorMsg)
			}
		case "gitlab":
			gl := gitlab.GitLabWrapper{
				Client: gitlab.NewGitLabClient(cGitToken),
			}

			// Check for existing base projects
			projects, err := gl.GetProjects()
			if err != nil {
				log.Fatal().Msgf("couldn't get gitlab projects: %s", err)
			}
			for _, repositoryName := range newRepositoryNames {
				found, err := gl.FindProjectInGroup(projects, repositoryName)
				if err != nil {
					log.Info().Msg(err.Error())
				}
				if found {
					return errors.New(fmt.Sprintf("project %s already exists and will need to be deleted before continuing", repositoryName))
				}
			}

			// Check for existing groups
			allgroups, err := gl.GetGroups()
			if err != nil {
				log.Fatal().Msgf("could not read gitlab groups: %s", err)
			}
			gid, err := gl.GetGroupID(allgroups, gitlabOwnerFlag)
			if err != nil {
				log.Fatal().Msgf("could not get group id for primary group: %s", err)
			}
			// Save for detokenize
			cGitlabOwnerGroupID = gid
			viper.Set("flags.gitlab-owner-group-id", cGitlabOwnerGroupID)
			viper.WriteConfig()
			subgroups, err := gl.GetSubGroups(gid)
			if err != nil {
				log.Fatal().Msgf("couldn't get gitlab projects: %s", err)
			}
			for _, teamName := range newTeamNames {
				for _, sg := range subgroups {
					if sg.Name == teamName {
						return errors.New(fmt.Sprintf("subgroup %s already exists and will need to be deleted before continuing", teamName))
					}
				}
			}
		}

		viper.Set(fmt.Sprintf("kubefirst-checks.%s-credentials", config.GitProvider), true)
		viper.WriteConfig()
		progressPrinter.IncrementTracker("preflight-checks", 1)
	} else {
		log.Info().Msg(fmt.Sprintf("already completed %s checks - continuing", config.GitProvider))
		progressPrinter.IncrementTracker("preflight-checks", 1)
	}

	executionControl = viper.GetBool("kubefirst-checks.kbot-setup")
	if !executionControl {

		log.Info().Msg("creating an ssh key pair for your new cloud infrastructure")
		sshPrivateKey, sshPublicKey, err = internalssh.CreateSshKeyPair()
		if err != nil {
			return err
		}
		if len(kbotPasswordFlag) == 0 {
			kbotPasswordFlag = pkg.Random(20)
		}
		log.Info().Msg("ssh key pair creation complete")

		viper.Set("kbot.password", kbotPasswordFlag)
		viper.Set("kbot.private-key", sshPrivateKey)
		viper.Set("kbot.public-key", sshPublicKey)
		viper.Set("kbot.username", "kbot")
		viper.Set("kubefirst-checks.kbot-setup", true)
		viper.WriteConfig()
		log.Info().Msg("kbot-setup complete")
		progressPrinter.IncrementTracker("preflight-checks", 1)
	} else {
		log.Info().Msg("already setup kbot user - continuing")
		progressPrinter.IncrementTracker("preflight-checks", 1)
	}

	log.Info().Msg("validation and kubefirst cli environment check is complete")

	if useTelemetryFlag {
		if err := wrappers.SendSegmentIoTelemetry(domainNameFlag, pkg.MetricInitCompleted, civo.CloudProvider, config.GitProvider, clusterId); err != nil {
			log.Info().Msg(err.Error())
			return err
		}
	}

	//* generate public keys for ssh
	if useTelemetryFlag {
		if err := wrappers.SendSegmentIoTelemetry(domainNameFlag, pkg.MetricMgmtClusterInstallStarted, civo.CloudProvider, config.GitProvider, clusterId); err != nil {
			log.Info().Msg(err.Error())
			return err
		}
	}

	publicKeys, err := ssh.NewPublicKeys("git", []byte(viper.GetString("kbot.private-key")), "")
	if err != nil {
		log.Info().Msgf("generate public keys failed: %s\n", err.Error())
	}

	//* emit cluster install started
	if useTelemetryFlag {
		if err := wrappers.SendSegmentIoTelemetry(domainNameFlag, pkg.MetricMgmtClusterInstallStarted, civo.CloudProvider, config.GitProvider, clusterId); err != nil {
			log.Info().Msg(err.Error())
		}
	}

	//* download dependencies to `$HOME/.k1/tools`
	if !viper.GetBool("kubefirst-checks.tools-downloaded") {
		log.Info().Msg("installing kubefirst dependencies")

		err := downloadManager.CivoDownloadTools(
			config.HelmClient,
			civo.HelmClientVersion,
			config.KubectlClient,
			civo.KubectlClientVersion,
			civo.LocalhostOS,
			civo.LocalhostArch,
			civo.TerraformClientVersion,
			config.ToolsDir,
		)
		if err != nil {
			return err
		}

		log.Info().Msg("download dependencies `$HOME/.k1/tools` complete")
		viper.Set("kubefirst-checks.tools-downloaded", true)
		viper.WriteConfig()
		progressPrinter.IncrementTracker("platform-create", 1)
	} else {
		log.Info().Msg("already completed download of dependencies to `$HOME/.k1/tools` - continuing")
		progressPrinter.IncrementTracker("platform-create", 1)
	}

	//* git clone and detokenize the gitops repository
	// todo improve this logic for removing `kubefirst clean`
	// if !viper.GetBool("template-repo.gitops.cloned") || viper.GetBool("template-repo.gitops.removed") {
	if !viper.GetBool("kubefirst-checks.gitops-ready-to-push") {

		log.Info().Msg("generating your new gitops repository")
		gitopsRepo, err := gitClient.CloneRefSetMain(gitopsTemplateBranchFlag, config.GitopsDir, gitopsTemplateURLFlag)
		if err != nil {
			log.Info().Msgf("error opening repo at: %s", config.GitopsDir)
		}
		log.Info().Msg("gitops repository clone complete")

		err = civo.CivoAdjustGitopsTemplateContent(civo.CloudProvider, clusterNameFlag, clusterTypeFlag, config.GitProvider, config.K1Dir, config.GitopsDir)
		if err != nil {
			return err
		}

		err = civo.DetokenizeCivoGitGitops(config.GitopsDir, &gitopsDirectoryTokens)
		if err != nil {
			return err
		}

		// Shim to adjust provider-specific files
		switch config.GitProvider {
		case "gitlab":
			err = civo.DetokenizeCivoAdditionalPath(config.ArgoWorkflowsDir, &civo.GitOpsDirectoryValues{GitlabOwner: cGitOwner})
			if err != nil {
				return err
			}

		}

		err = gitClient.AddRemote(config.DestinationGitopsRepoGitURL, config.GitProvider, gitopsRepo)
		if err != nil {
			return err
		}

		err = gitClient.Commit(gitopsRepo, "committing initial detokenized gitops-template repo content")
		if err != nil {
			return err
		}

		// todo emit init telemetry end
		viper.Set("kubefirst-checks.gitops-ready-to-push", true)
		viper.WriteConfig()
		progressPrinter.IncrementTracker("platform-create", 1)
	} else {
		log.Info().Msg("already completed gitops repo generation - continuing")
		progressPrinter.IncrementTracker("platform-create", 1)
	}

	switch config.GitProvider {
	case "github":
		// //* create teams and repositories in github
		executionControl = viper.GetBool("kubefirst-checks.terraform-apply-github")
		if !executionControl {
			log.Info().Msg("Creating github resources with terraform")

			tfEntrypoint := config.GitopsDir + "/terraform/github"
			tfEnvs := map[string]string{}
			tfEnvs = civo.GetGithubTerraformEnvs(tfEnvs)
			err := terraform.InitApplyAutoApprove(dryRunFlag, tfEntrypoint, tfEnvs)
			if err != nil {
				return errors.New(fmt.Sprintf("error creating github resources with terraform %s: %s", tfEntrypoint, err))
			}

			log.Info().Msgf("Created git repositories and teams for github.com/%s", cGitOwner)
			viper.Set("kubefirst-checks.terraform-apply-github", true)
			viper.WriteConfig()
		} else {
			log.Info().Msg("already created github terraform resources")
			progressPrinter.IncrementTracker("platform-create", 1)
		}
	case "gitlab":
		// //* create teams and repositories in gitlab
		gl := gitlab.GitLabWrapper{
			Client: gitlab.NewGitLabClient(cGitToken),
		}
		allgroups, err := gl.GetGroups()
		if err != nil {
			log.Fatal().Msgf("could not read gitlab groups: %s", err)
		}
		gid, err := gl.GetGroupID(allgroups, gitlabOwnerFlag)
		if err != nil {
			log.Fatal().Msgf("could not get group id for primary group: %s", err)
		}
		executionControl = viper.GetBool("kubefirst-checks.terraform-apply-gitlab")
		if !executionControl {
			log.Info().Msg("Creating gitlab resources with terraform")

			tfEntrypoint := config.GitopsDir + "/terraform/gitlab"
			tfEnvs := map[string]string{}
			tfEnvs = civo.GetGitlabTerraformEnvs(tfEnvs, gid)
			err := terraform.InitApplyAutoApprove(dryRunFlag, tfEntrypoint, tfEnvs)
			if err != nil {
				return errors.New(fmt.Sprintf("error creating gitlab resources with terraform %s: %s", tfEntrypoint, err))
			}

			log.Info().Msgf("created git projects and groups for gitlab.com/%s", gitlabOwnerFlag)
			viper.Set("kubefirst-checks.terraform-apply-gitlab", true)
			viper.WriteConfig()
		} else {
			log.Info().Msg("already created gitlab terraform resources")
			progressPrinter.IncrementTracker("platform-create", 1)
		}
	}

	//* push detokenized gitops-template repository content to new remote
	executionControl = viper.GetBool("kubefirst-checks.gitops-repo-pushed")
	if !executionControl {
		gitopsRepo, err := git.PlainOpen(config.GitopsDir)
		if err != nil {
			log.Info().Msgf("error opening repo at: %s", config.GitopsDir)
		}

		// For GitLab, we currently need to add an ssh key to the authenticating user
		if config.GitProvider == "gitlab" {
			gl := gitlab.GitLabWrapper{
				Client: gitlab.NewGitLabClient(cGitToken),
			}
			keys, err := gl.GetUserSSHKeys()
			if err != nil {
				log.Fatal().Msgf("unable to check for ssh keys in gitlab: %s", err.Error())
			}

			var keyName = "kubefirst-bot-ssh-key"
			var keyFound bool = false
			for _, key := range keys {
				if key.Title == keyName {
					if strings.Contains(key.Key, strings.TrimSuffix(viper.GetString("kbot.public-key"), "\n")) {
						log.Info().Msgf("ssh key %s already exists and key is up to date, continuing", keyName)
						keyFound = true
					} else {
						log.Fatal().Msgf("ssh key %s already exists and key data has drifted - please remove before continuing", keyName)
					}
				}
			}
			if !keyFound {
				log.Info().Msgf("creating ssh key %s...", keyName)
				err := gl.AddUserSSHKey(keyName, viper.GetString("kbot.public-key"))
				if err != nil {
					log.Fatal().Msgf("error adding ssh key %s: %s", keyName, err.Error())
				}
				viper.Set("kbot.gitlab-user-based-ssh-key-title", keyName)
				viper.WriteConfig()
			}
		}

		// Push gitops repo to remote
		err = gitopsRepo.Push(&git.PushOptions{
			RemoteName: config.GitProvider,
			Auth:       publicKeys,
		})
		if err != nil {
			log.Panic().Msgf("error pushing detokenized gitops repository to remote %s: %s", config.DestinationGitopsRepoGitURL, err)
		}

		log.Info().Msgf("successfully pushed gitops to git@github.com/%s/gitops", cGitOwner)
		// todo delete the local gitops repo and re-clone it
		// todo that way we can stop worrying about which origin we're going to push to
		log.Info().Msgf("successfully pushed gitops to git@g%s/%s/gitops", cGitHost, cGitOwner)
		viper.Set("kubefirst-checks.gitops-repo-pushed", true)
		viper.WriteConfig()
		progressPrinter.IncrementTracker("platform-create", 1)
	} else {
		log.Info().Msg("already pushed detokenized gitops repository content")
		progressPrinter.IncrementTracker("platform-create", 1)
	}

	metaphorTemplateTokens := civo.MetaphorTokenValues{
		CheckoutCWFTTemplate:                  "git-checkout-with-gitops-ssh",
		CloudRegion:                           cloudRegionFlag,
		ClusterName:                           clusterNameFlag,
		CommitCWFTTemplate:                    "git-commit-ssh",
		ContainerRegistryURL:                  fmt.Sprintf("%s/%s/metaphor-frontend", containerRegistryHost, cGitOwner),
		DomainName:                            domainNameFlag,
		MetaphorFrontendDevelopmentIngressURL: fmt.Sprintf("metaphor-development.%s", domainNameFlag),
		MetaphorFrontendProductionIngressURL:  fmt.Sprintf("metaphor-production.%s", domainNameFlag),
		MetaphorFrontendStagingIngressURL:     fmt.Sprintf("metaphor-staging.%s", domainNameFlag),
	}

	//* git clone and detokenize the metaphor-frontend-template repository
	if !viper.GetBool("kubefirst-checks.metaphor-repo-pushed") {

		if configs.K1Version != "" {
			gitopsTemplateBranchFlag = configs.K1Version
		}

		log.Info().Msg("generating your new metaphor-frontend repository")
		metaphorRepo, err := gitClient.CloneRefSetMain(metaphorTemplateBranchFlag, config.MetaphorDir, metaphorTemplateURLFlag)
		if err != nil {
			log.Info().Msgf("error opening repo at: %s", config.MetaphorDir)
		}

		log.Info().Msg("metaphor repository clone complete")

		err = civo.CivoAdjustMetaphorTemplateContent(config.GitProvider, config.K1Dir, config.MetaphorDir)
		if err != nil {
			return err
		}

		err = civo.DetokenizeCivoGitMetaphor(config.MetaphorDir, &metaphorTemplateTokens)
		if err != nil {
			return err
		}
		err = gitClient.AddRemote(config.DestinationMetaphorRepoGitURL, config.GitProvider, metaphorRepo)
		if err != nil {
			return err
		}

		err = gitClient.Commit(metaphorRepo, "committing detokenized metaphor-frontend-template repo content")
		if err != nil {
			return err
		}

		err = metaphorRepo.Push(&git.PushOptions{
			RemoteName: config.GitProvider,
			Auth:       publicKeys,
		})
		if err != nil {
			log.Panic().Msgf("error pushing detokenized gitops repository to remote %s", config.DestinationMetaphorRepoGitURL)
		}

		log.Info().Msgf("successfully pushed gitops to git@%s/%s/metaphor-frontend", cGitHost, cGitOwner)
		// todo delete the local gitops repo and re-clone it
		// todo that way we can stop worrying about which origin we're going to push to
		log.Info().Msgf("pushed detokenized metaphor-frontend repository to %s/%s", cGitHost, cGitOwner)

		viper.Set("kubefirst-checks.metaphor-repo-pushed", true)
		viper.WriteConfig()
		progressPrinter.IncrementTracker("platform-create", 1)
	} else {
		log.Info().Msg("already completed gitops repo generation - continuing")
		progressPrinter.IncrementTracker("platform-create", 1)
	}

	//* create civo cloud resources
	if !viper.GetBool("kubefirst-checks.terraform-apply-civo") {
		log.Info().Msg("Creating civo cloud resources with terraform")

		tfEntrypoint := config.GitopsDir + "/terraform/civo"
		tfEnvs := map[string]string{}
		tfEnvs = civo.GetCivoTerraformEnvs(tfEnvs)
		err := terraform.InitApplyAutoApprove(dryRunFlag, tfEntrypoint, tfEnvs)
		if err != nil {
			return errors.New(fmt.Sprintf("error creating civo resources with terraform %s : %s", tfEntrypoint, err))
		}

		log.Info().Msg("Created civo cloud resources")
		viper.Set("kubefirst-checks.terraform-apply-civo", true)
		viper.WriteConfig()
		progressPrinter.IncrementTracker("platform-create", 1)
	} else {
		log.Info().Msg("already created github terraform resources")
		progressPrinter.IncrementTracker("platform-create", 1)
	}

	clientset, err := k8s.GetClientSet(dryRunFlag, config.Kubeconfig)
	if err != nil {
		return err
	}

	// kubernetes.BootstrapSecrets
	// todo there is a secret condition in AddK3DSecrets to this not checked
	// todo deconstruct CreateNamespaces / CreateSecret
	// todo move secret structs to constants to be leveraged by either local or civo
	executionControl = viper.GetBool("kubefirst-checks.k8s-secrets-created")
	if !executionControl {
		err := civo.BootstrapCivoMgmtCluster(dryRunFlag, config.Kubeconfig, config.GitProvider, cGitUser)
		if err != nil {
			log.Info().Msg("Error adding kubernetes secrets for bootstrap")
			return err
		}
		viper.Set("kubefirst-checks.k8s-secrets-created", true)
		viper.WriteConfig()
		progressPrinter.IncrementTracker("platform-create", 1)
	} else {
		log.Info().Msg("already added secrets to civo cluster")
		progressPrinter.IncrementTracker("platform-create", 1)
	}

	//* check for ssl restore
	log.Info().Msg("checking for tls secrets to restore")
	secretsFilesToRestore, err := ioutil.ReadDir(config.SSLBackupDir + "/secrets")
	if err != nil {
		log.Info().Msgf("%s", err)
	}
	if len(secretsFilesToRestore) != 0 {
		// todo would like these but requires CRD's and is not currently supported
		// add crds ( use execShellReturnErrors? )
		// https://raw.githubusercontent.com/cert-manager/cert-manager/v1.11.0/deploy/crds/crd-clusterissuers.yaml
		// https://raw.githubusercontent.com/cert-manager/cert-manager/v1.11.0/deploy/crds/crd-certificates.yaml
		// add certificates, and clusterissuers
		log.Info().Msgf("found %d tls secrets to restore", len(secretsFilesToRestore))
		ssl.Restore(config.SSLBackupDir, domainNameFlag, config.Kubeconfig)
	} else {
		log.Info().Msg("no files found in secrets directory, continuing")
	}

	// Handle secret creation for buildkit
	createTokensFor := []string{"metaphor-frontend"}
	switch config.GitProvider {
	// GitHub docker auth secret
	// Buildkit requires a specific format for Docker auth created as a secret
	// For GitHub, this becomes the provided token (pat)
	case "github":
		usernamePasswordString := fmt.Sprintf("%s:%s", cGitUser, cGitToken)
		usernamePasswordStringB64 := base64.StdEncoding.EncodeToString([]byte(usernamePasswordString))
		dockerConfigString := fmt.Sprintf(`{"auths": {"%s": {"username": "%s", "password": "%s", "email": "%s", "auth": "%s"}}}`, containerRegistryHost, cGitUser, cGitToken, "k-bot@example.com", usernamePasswordStringB64)

		for _, repository := range createTokensFor {
			// Create argo workflows pull secret
			// This is formatted to work with buildkit
			argoDeployTokenSecret := &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-deploy", repository), Namespace: "argo"},
				Data:       map[string][]byte{"config.json": []byte(dockerConfigString)},
				Type:       "Opaque",
			}
			err = k8s.CreateSecretV2(config.Kubeconfig, argoDeployTokenSecret)
			if err != nil {
				log.Error().Msgf("error while creating secret for repository deploy token: %s", err)
			}
		}
	// GitLab Deploy Tokens
	// Project deploy tokens are generated for each member of createTokensForProjects
	// These deploy tokens are used to authorize against the GitLab container registry
	case "gitlab":
		gl := gitlab.GitLabWrapper{
			Client: gitlab.NewGitLabClient(cGitToken),
		}

		for _, project := range createTokensFor {
			var p = gitlab.DeployTokenCreateParameters{
				Name:     fmt.Sprintf("%s-deploy", project),
				Username: fmt.Sprintf("%s-deploy", project),
				Scopes:   []string{"read_registry", "write_registry"},
			}

			log.Info().Msgf("creating project deploy token for project %s...", project)
			token, err := gl.CreateProjectDeployToken(project, &p)
			if err != nil {
				log.Fatal().Msgf("error creating project deploy token for project %s: %s", project, err)
			}

			if token != "" {
				log.Info().Msgf("creating secret for project deploy token for project %s...", project)
				usernamePasswordString := fmt.Sprintf("%s:%s", p.Username, token)
				usernamePasswordStringB64 := base64.StdEncoding.EncodeToString([]byte(usernamePasswordString))
				dockerConfigString := fmt.Sprintf(`{"auths": {"%s": {"username": "%s", "password": "%s", "email": "%s", "auth": "%s"}}}`, containerRegistryHost, p.Username, token, "k-bot@example.com", usernamePasswordStringB64)

				createInNamespace := []string{"development", "staging", "production"}
				for _, namespace := range createInNamespace {
					deployTokenSecret := &v1.Secret{
						ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-deploy", project), Namespace: namespace},
						Data:       map[string][]byte{".dockerconfigjson": []byte(dockerConfigString)},
						Type:       "kubernetes.io/dockerconfigjson",
					}
					err = k8s.CreateSecretV2(config.Kubeconfig, deployTokenSecret)
					if err != nil {
						log.Error().Msgf("error while creating secret for project deploy token: %s", err)
					}
				}

				// Create argo workflows pull secret
				// This is formatted to work with buildkit
				argoDeployTokenSecret := &v1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-deploy", project), Namespace: "argo"},
					Data:       map[string][]byte{"config.json": []byte(dockerConfigString)},
					Type:       "Opaque",
				}
				err = k8s.CreateSecretV2(config.Kubeconfig, argoDeployTokenSecret)
				if err != nil {
					log.Error().Msgf("error while creating secret for project deploy token: %s", err)
				}
			}
		}
	}

	//* helm add argo repository && update
	helmRepo := helm.HelmRepo{
		RepoName:     "argo",
		RepoURL:      "https://argoproj.github.io/argo-helm",
		ChartName:    "argo-cd",
		Namespace:    "argocd",
		ChartVersion: "4.10.5",
	}

	//* helm add repo and update
	executionControl = viper.GetBool("kubefirst-checks.argocd-helm-repo-added")
	if !executionControl {
		log.Info().Msgf("helm repo add %s %s and helm repo update", helmRepo.RepoName, helmRepo.RepoURL)
		helm.AddRepoAndUpdateRepo(dryRunFlag, config.HelmClient, helmRepo, config.Kubeconfig)
		log.Info().Msg("helm repo added")
		viper.Set("kubefirst-checks.argocd-helm-repo-added", true)
		viper.WriteConfig()
		progressPrinter.IncrementTracker("platform-create", 1)
	} else {
		log.Info().Msg("argo helm repository already added, continuing")
		progressPrinter.IncrementTracker("platform-create", 1)
	}
	//* helm install argocd
	executionControl = viper.GetBool("kubefirst-checks.argocd-helm-install")
	if !executionControl {
		log.Info().Msgf("helm install %s and wait", helmRepo.RepoName)
		// todo adopt golang helm client for helm install
		err := helm.Install(dryRunFlag, config.HelmClient, helmRepo, config.Kubeconfig)
		if err != nil {
			return err
		}
		viper.Set("kubefirst-checks.argocd-helm-install", true)
		viper.WriteConfig()
		progressPrinter.IncrementTracker("platform-create", 1)
	} else {
		log.Info().Msg("argo helm already installed, continuing")
		progressPrinter.IncrementTracker("platform-create", 1)
	}

	// Wait for ArgoCD StatefulSet Pods to transition to Running
	argoCDStatefulSet, err := k8s.ReturnStatefulSetObject(
		config.Kubeconfig,
		"app.kubernetes.io/part-of",
		"argocd",
		"argocd",
		60,
	)
	if err != nil {
		log.Info().Msgf("Error finding ArgoCD StatefulSet: %s", err)
	}
	_, err = k8s.WaitForStatefulSetReady(config.Kubeconfig, argoCDStatefulSet, 90, false)
	if err != nil {
		log.Info().Msgf("Error waiting for ArgoCD StatefulSet ready state: %s", err)
	}

	//* ArgoCD port-forward
	argoCDStopChannel := make(chan struct{}, 1)
	defer func() {
		close(argoCDStopChannel)
	}()
	k8s.OpenPortForwardPodWrapper(
		config.Kubeconfig,
		"argocd-server", // todo fix this, it should `argocd
		"argocd",
		8080,
		8080,
		argoCDStopChannel,
	)
	log.Info().Msgf("port-forward to argocd is available at %s", civo.ArgocdPortForwardURL)

	//* argocd pods are ready, get and set credentials
	executionControl = viper.GetBool("kubefirst-checks.argocd-credentials-set")
	if !executionControl {
		log.Info().Msg("Setting argocd username and password credentials")

		argocd.ArgocdSecretClient = clientset.CoreV1().Secrets("argocd")

		argocdPassword := k8s.GetSecretValue(argocd.ArgocdSecretClient, "argocd-initial-admin-secret", "password")
		if argocdPassword == "" {
			log.Info().Msg("argocd password not found in secret")
			return err
		}

		viper.Set("components.argocd.password", argocdPassword)
		viper.Set("components.argocd.username", "admin")
		viper.WriteConfig()
		log.Info().Msg("argocd username and password credentials set successfully")

		log.Info().Msg("Getting an argocd auth token")
		// todo return in here and pass argocdAuthToken as a parameter
		token, err := argocd.GetArgoCDToken("admin", argocdPassword)
		if err != nil {
			return err
		}

		log.Info().Msg("argocd admin auth token set")

		viper.Set("components.argocd.auth-token", token)
		viper.Set("kubefirst-checks.argocd-credentials-set", true)
		viper.WriteConfig()
		progressPrinter.IncrementTracker("platform-create", 1)
	} else {
		log.Info().Msg("argo credentials already set, continuing")
		progressPrinter.IncrementTracker("platform-create", 1)
	}

	//* argocd sync registry and start sync waves
	executionControl = viper.GetBool("kubefirst-checks.argocd-create-registry")
	if !executionControl {
		log.Info().Msg("applying the registry application to argocd")
		registryYamlPath := fmt.Sprintf("%s/gitops/registry/%s/registry.yaml", config.K1Dir, clusterNameFlag)
		_, _, err := pkg.ExecShellReturnStrings(config.KubectlClient, "--kubeconfig", config.Kubeconfig, "-n", "argocd", "apply", "-f", registryYamlPath, "--wait")
		if err != nil {
			log.Warn().Msgf("failed to execute kubectl apply -f %s: error %s", registryYamlPath, err.Error())
			return err
		}
		viper.Set("kubefirst-checks.argocd-create-registry", true)
		viper.WriteConfig()
		progressPrinter.IncrementTracker("platform-create", 1)
	} else {
		log.Info().Msg("argocd registry create already done, continuing")
		progressPrinter.IncrementTracker("platform-create", 1)
	}

	// Wait for Vault StatefulSet Pods to transition to Running
	vaultStatefulSet, err := k8s.ReturnStatefulSetObject(
		config.Kubeconfig,
		"app.kubernetes.io/instance",
		"vault",
		"vault",
		60,
	)
	if err != nil {
		log.Info().Msgf("Error finding Vault StatefulSet: %s", err)
	}
	_, err = k8s.WaitForStatefulSetReady(config.Kubeconfig, vaultStatefulSet, 60, true)
	if err != nil {
		log.Info().Msgf("Error waiting for Vault StatefulSet ready state: %s", err)
	}
	progressPrinter.IncrementTracker("platform-create", 1)

	//* vault port-forward
	vaultStopChannel := make(chan struct{}, 1)
	defer func() {
		close(vaultStopChannel)
	}()
	k8s.OpenPortForwardPodWrapper(
		config.Kubeconfig,
		"vault-0",
		"vault",
		8200,
		8200,
		vaultStopChannel,
	)

	// Init and unseal vault
	fmt.Println("Initializing and unsealing Vault...")
	executionControl = viper.GetBool("kubefirst-checks.vault-initialized")
	if !executionControl {
		// Initialize and unseal Vault
		err = vault.UnsealVault(config.Kubeconfig, &vault.VaultUnsealOptions{
			VaultAPIAddress:      "http://localhost:8200",
			HighAvailability:     true,
			HighAvailabilityType: "raft",
			RaftLeader:           true,
			RaftFollower:         false,
			UseAPI:               true,
		})
		if err != nil {
			log.Fatal().Msgf("could not initialize vault: %s", err)
		}

		err = vault.UnsealVault(config.Kubeconfig, &vault.VaultUnsealOptions{
			HighAvailability:     true,
			HighAvailabilityType: "raft",
			Nodes:                3,
			RaftLeader:           false,
			RaftFollower:         true,
			UseAPI:               false,
		})
		if err != nil {
			log.Fatal().Msgf("could not initialize vault: %s", err)
		}
		viper.Set("kubefirst-checks.vault-initialized", true)
		viper.WriteConfig()
	} else {
		log.Info().Msg("vault is already initialized - skipping")
	}

	// Final progress printer
	progressPrinter.AddTracker("finalizing", "Verifying and wrapping up", 2)
	progressPrinter.SetupProgress(progressPrinter.TotalOfTrackers(), false)

	//* configure vault with terraform
	executionControl = viper.GetBool("kubefirst-checks.terraform-apply-vault")
	if !executionControl {
		// todo evaluate progressPrinter.IncrementTracker("step-vault", 1)

		//* run vault terraform
		log.Info().Msg("configuring vault with terraform")

		tfEnvs := map[string]string{}

		tfEnvs = civo.GetVaultTerraformEnvs(config, tfEnvs)
		tfEnvs = civo.GetCivoTerraformEnvs(tfEnvs)
		tfEntrypoint := config.GitopsDir + "/terraform/vault"
		err := terraform.InitApplyAutoApprove(dryRunFlag, tfEntrypoint, tfEnvs)
		if err != nil {
			return err
		}

		log.Info().Msg("vault terraform executed successfully")
		viper.Set("kubefirst-checks.terraform-apply-vault", true)
		viper.WriteConfig()
		progressPrinter.IncrementTracker("finalizing", 1)
	} else {
		log.Info().Msg("already executed vault terraform")
		progressPrinter.IncrementTracker("finalizing", 1)
	}

	//* create users
	executionControl = viper.GetBool("kubefirst-checks.terraform-apply-users")
	if !executionControl {
		log.Info().Msg("applying users terraform")

		tfEnvs := map[string]string{}
		tfEnvs = civo.GetCivoTerraformEnvs(tfEnvs)
		tfEnvs = civo.GetUsersTerraformEnvs(config, tfEnvs)
		tfEntrypoint := config.GitopsDir + "/terraform/users"
		err := terraform.InitApplyAutoApprove(dryRunFlag, tfEntrypoint, tfEnvs)
		if err != nil {
			return err
		}
		log.Info().Msg("executed users terraform successfully")
		// progressPrinter.IncrementTracker("step-users", 1)
		viper.Set("kubefirst-checks.terraform-apply-users", true)
		viper.WriteConfig()
		progressPrinter.IncrementTracker("finalizing", 1)
	} else {
		log.Info().Msg("already created users with terraform")
		progressPrinter.IncrementTracker("finalizing", 1)
	}

	// Wait for console Deployment Pods to transition to Running
	consoleDeployment, err := k8s.ReturnDeploymentObject(
		config.Kubeconfig,
		"app.kubernetes.io/instance",
		"kubefirst-console",
		"kubefirst",
		60,
	)
	if err != nil {
		log.Info().Msgf("Error finding console Deployment: %s", err)
	}
	_, err = k8s.WaitForDeploymentReady(config.Kubeconfig, consoleDeployment, 120)
	if err != nil {
		log.Info().Msgf("Error waiting for console Deployment ready state: %s", err)
	}

	//* console port-forward
	consoleStopChannel := make(chan struct{}, 1)
	defer func() {
		close(consoleStopChannel)
	}()
	k8s.OpenPortForwardPodWrapper(
		config.Kubeconfig,
		"kubefirst-console",
		"kubefirst",
		8080,
		9094,
		consoleStopChannel,
	)

	log.Info().Msg("kubefirst installation complete")
	log.Info().Msg("welcome to your new kubefirst platform powered by Civo cloud")

	err = pkg.IsConsoleUIAvailable(pkg.KubefirstConsoleLocalURLCloud)
	if err != nil {
		log.Error().Err(err).Msg("")
	}

	err = pkg.OpenBrowser(pkg.KubefirstConsoleLocalURLCloud)
	if err != nil {
		log.Error().Err(err).Msg("")
	}

	// todo: fix argo pw
	// this is probably going to get streamlined later, but this is necessary now
	reports.CivoHandoffScreen(viper.GetString("components.argocd.password"), clusterNameFlag, domainNameFlag, cGitOwner, config, dryRunFlag, false)

	if useTelemetryFlag {
		if err := wrappers.SendSegmentIoTelemetry(domainNameFlag, pkg.MetricMgmtClusterInstallCompleted, civo.CloudProvider, config.GitProvider, clusterId); err != nil {
			log.Info().Msg(err.Error())
			return err
		}
	}

	time.Sleep(time.Millisecond * 200) // allows progress bars to finish

	return nil
}
