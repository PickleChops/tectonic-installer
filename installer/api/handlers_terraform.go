package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/dghubble/sessions"
	"github.com/kardianos/osext"

	"github.com/coreos/tectonic-installer/installer/pkg/tectonic"
	"github.com/coreos/tectonic-installer/installer/pkg/terraform"
)

const (
	bcryptCost = 12
)

// TerraformApplyHandlerInput describes the input expected by the
// terraformApplyHandler HTTP Handler.
type TerraformApplyHandlerInput struct {
	Platform      string                 `json:"platform"`
	Credentials   terraform.Credentials  `json:"credentials"`
	AdminPassword []byte                 `json:"adminPassword"`
	Variables     map[string]interface{} `json:"variables"`
	License       string                 `json:"license"`
	PullSecret    string                 `json:"pullSecret"`
	DryRun        bool                   `json:"dryRun"`
	Retry         bool                   `json:"retry"`
}

func terraformApplyHandler(w http.ResponseWriter, req *http.Request, ctx *Context) error {
	// Read the input from the request's body.
	var input TerraformApplyHandlerInput
	if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
		return newBadRequestError("Could not unmarshal input: %s", err)
	}
	defer req.Body.Close()

	var ex *terraform.Executor
	var err error
	if input.Retry {
		// Restore the execution environment from the session.
		_, ex, _, err = restoreExecutionFromSession(req, ctx.Sessions, &input.Credentials)
	} else {
		// Create a new Terraform Executor with the TF variables.
		ex, err = newExecutorFromApplyHandlerInput(&input)
	}
	if err != nil {
		return err
	}
	tfMainDir := fmt.Sprintf("%s/platforms/%s", ex.WorkingDirectory(), input.Platform)

	// Copy the TF Templates to the Executor's working directory.
	if err := terraform.RestoreSources(ex.WorkingDirectory()); err != nil {
		return newInternalServerError("could not write Terraform templates: %s", err)
	}

	// Choose to run 'get' or 'init' based on Terraform version.
	sub10Rx := regexp.MustCompile("^Terraform v0\\.[0-9]\\.[0-9]+")
	out, err := exec.Command("terraform", "version").Output()
	if err != nil {
		return newInternalServerError("Failed to determine Terraform version: %s", err)
	}
	prepCommand := "init"
	if sub10Rx.Match(out) {
		prepCommand = "get"
	}

	// Execute Terraform get or init and wait for it to finish.
	_, prepDone, err := ex.Execute(prepCommand, "-no-color", tfMainDir)
	if err != nil {
		return newInternalServerError("Failed to run Terraform (%s): %s", prepCommand, err)
	}
	<-prepDone

	// Store both the path to the Executor and the ID of the execution so that
	// the status can be read later on.
	session := ctx.Sessions.New(installerSessionName)
	session.Values["terraform_path"] = ex.WorkingDirectory()

	var id int
	var action string
	if input.DryRun {
		id, _, err = ex.Execute("plan", "-no-color", tfMainDir)
		action = "show"
	} else {
		id, _, err = ex.Execute("apply", "-input=false", "-no-color", tfMainDir)
		action = "apply"
	}
	if err != nil {
		return newInternalServerError("Failed to run Terraform (%s): %s", action, err)
	}
	session.Values["terraform_id"] = id
	session.Values["action"] = action

	if err := ctx.Sessions.Save(w, session); err != nil {
		return newInternalServerError("Failed to save session: %s", err)
	}

	return nil
}

func terraformAssetsHandler(w http.ResponseWriter, req *http.Request, ctx *Context) error {
	// Restore the execution environment from the session.
	_, ex, _, err := restoreExecutionFromSession(req, ctx.Sessions, nil)
	if err != nil {
		return err
	}

	// Stream the assets as a ZIP.
	w.Header().Set("Content-Type", "application/zip")
	if err := ex.Zip(w, true); err != nil {
		return newInternalServerError("Could not archive assets: %s", err)
	}
	return nil
}

// TerraformDestroyHandlerInput describes the input expected by the
// terraformDestroyHandler HTTP Handler.
type TerraformDestroyHandlerInput struct {
	Platform    string                `json:"platform"`
	Credentials terraform.Credentials `json:"credentials"`
}

func terraformDestroyHandler(w http.ResponseWriter, req *http.Request, ctx *Context) error {
	// Read the input from the request's body.
	var input TerraformDestroyHandlerInput
	if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
		return newBadRequestError("Could not unmarshal input: %s", err)
	}
	defer req.Body.Close()

	// Restore the execution environment from the session.
	_, ex, _, err := restoreExecutionFromSession(req, ctx.Sessions, &input.Credentials)
	if err != nil {
		return err
	}
	tfMainDir := fmt.Sprintf("%s/platforms/%s", ex.WorkingDirectory(), input.Platform)

	// Execute Terraform apply in the background.
	id, _, err := ex.Execute("destroy", "-force", "-no-color", tfMainDir)
	if err != nil {
		return newInternalServerError("Failed to run Terraform (apply): %s", err)
	}

	// Store both the path to the Executor and the ID of the execution so that
	// the status can be read later on.
	session := ctx.Sessions.New(installerSessionName)
	session.Values["action"] = "destroy"
	session.Values["terraform_path"] = ex.WorkingDirectory()
	session.Values["terraform_id"] = id
	if err := ctx.Sessions.Save(w, session); err != nil {
		return newInternalServerError("Failed to save session: %s", err)
	}
	return nil
}

// newExecutorFromApplyHandlerInput creates a new Executor based on the given
// TerraformApplyHandlerInput.
func newExecutorFromApplyHandlerInput(input *TerraformApplyHandlerInput) (*terraform.Executor, error) {
	// Construct the path where the Executor should run based on the the cluster
	// name and current's binary path.
	binaryPath, err := osext.ExecutableFolder()
	if err != nil {
		return nil, newInternalServerError("Could not determine executable's folder: %s", err)
	}
	clusterName := input.Variables["tectonic_cluster_name"].(string)
	if len(clusterName) == 0 {
		return nil, newBadRequestError("Tectonic cluster name not provided")
	}
	exPath := filepath.Join(binaryPath, "clusters", clusterName+time.Now().Format("_2006-01-02_15-04-05"))

	// Create a new Executor.
	ex, err := terraform.NewExecutor(exPath)
	if err != nil {
		return nil, newInternalServerError("Could not create Terraform executor: %s", err)
	}

	// Write the License and Pull Secret to disk, and wire these files in the
	// variables.
	if input.License == "" {
		return nil, newBadRequestError("Tectonic license not provided")
	}
	ex.AddFile("license.txt", []byte(input.License))
	if input.PullSecret == "" {
		return nil, newBadRequestError("Tectonic pull secret not provided")
	}
	ex.AddFile("pull_secret.json", []byte(input.PullSecret))
	input.Variables["tectonic_license_path"] = "./license.txt"
	input.Variables["tectonic_pull_secret_path"] = "./pull_secret.json"
	serviceCidr := input.Variables["tectonic_service_cidr"].(string)

	ip, ok := input.Variables["tectonic_kube_apiserver_service_ip"].(string)
	if !ok || len(ip) == 0 {
		input.Variables["tectonic_kube_apiserver_service_ip"], err = tectonic.APIServiceIP(serviceCidr)
		if err != nil {
			return nil, newInternalServerError("Error calculating service IP: %s", err)
		}
	}

	ip, ok = input.Variables["tectonic_kube_dns_service_ip"].(string)
	if !ok || len(ip) == 0 {
		input.Variables["tectonic_kube_dns_service_ip"], err = tectonic.DNSServiceIP(serviceCidr)
		if err != nil {
			return nil, newInternalServerError("Error calculating DNS IP: %s", err)
		}
	}

	ip, ok = input.Variables["tectonic_kube_etcd_service_ip"].(string)
	if !ok || len(ip) == 0 {
		input.Variables["tectonic_kube_etcd_service_ip"], err = tectonic.EtcdServiceIP(serviceCidr)
		if err != nil {
			return nil, newInternalServerError("Error calculating etcd service IP: %s", err)
		}
	}

	// Add variables and the required environment variables.
	if variables, err := json.MarshalIndent(input.Variables, "", "  "); err == nil {
		ex.AddVariables(variables)
	} else {
		return nil, newBadRequestError("Could not marshal Terraform variables: %s", err)
	}
	if err := ex.AddCredentials(&input.Credentials); err != nil {
		return nil, newBadRequestError("Could not validate Terraform credentials: %v", err)
	}

	return ex, nil
}

// restoreExecutionFromSession tries to re-create an existing Executor based on
// the data available in session and the provided credentials.
func restoreExecutionFromSession(req *http.Request, sessionProvider sessions.Store, credentials *terraform.Credentials) (*sessions.Session, *terraform.Executor, int, error) {
	session, err := sessionProvider.Get(req, installerSessionName)
	if err != nil {
		return nil, nil, -1, newNotFoundError("Could not find session data. Run terraform apply first.")
	}
	executionPath, ok := session.Values["terraform_path"]
	if !ok {
		return nil, nil, -1, newNotFoundError("Could not find terraform_path in session. Run terraform apply first.")
	}
	executionID, ok := session.Values["terraform_id"]
	if !ok {
		return nil, nil, -1, newNotFoundError("Could not find terraform_id in session. Run terraform apply first.")
	}
	ex, err := terraform.NewExecutor(executionPath.(string))
	if err != nil {
		return nil, nil, -1, newInternalServerError("Could not create Terraform executor")
	}
	if err := ex.AddCredentials(credentials); err != nil {
		return nil, nil, -1, newBadRequestError("Could not validate Terraform credentials")
	}
	return session, ex, executionID.(int), nil
}
