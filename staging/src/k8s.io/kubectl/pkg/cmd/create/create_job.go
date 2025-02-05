/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package create

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/resource"
	batchv1client "k8s.io/client-go/kubernetes/typed/batch/v1"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
	"k8s.io/utils/ptr"
)

var (
	jobLong = templates.LongDesc(i18n.T(`
		Create a job with the specified name.`))

	jobExample = templates.Examples(i18n.T(`
		# Create a job
		kubectl create job my-job --image=busybox

		# Create a job with a command
		kubectl create job my-job --image=busybox -- date

		# Create a job from a cron job named "a-cronjob"
		kubectl create job test-job --from=cronjob/a-cronjob`))
)

// CreateJobOptions is the command line options for 'create job'
type CreateJobOptions struct {
	PrintFlags *genericclioptions.PrintFlags

	PrintObj func(obj runtime.Object) error

	Name    string
	Image   string
	From    string
	Command []string

	Namespace           string
	EnforceNamespace    bool
	Client              batchv1client.BatchV1Interface
	DryRunStrategy      cmdutil.DryRunStrategy
	ValidationDirective string
	Builder             *resource.Builder
	FieldManager        string
	CreateAnnotation    bool

	genericiooptions.IOStreams
}

// NewCreateJobOptions initializes and returns new CreateJobOptions instance
func NewCreateJobOptions(ioStreams genericiooptions.IOStreams) *CreateJobOptions {
	return &CreateJobOptions{
		PrintFlags: genericclioptions.NewPrintFlags("created").WithTypeSetter(scheme.Scheme),
		IOStreams:  ioStreams,
	}
}

// NewCmdCreateJob is a command to ease creating Jobs from CronJobs.
func NewCmdCreateJob(f cmdutil.Factory, ioStreams genericiooptions.IOStreams) *cobra.Command {
	o := NewCreateJobOptions(ioStreams)
	cmd := &cobra.Command{
		Use:                   "job NAME --image=image [--from=cronjob/name] -- [COMMAND] [args...]",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Create a job with the specified name"),
		Long:                  jobLong,
		Example:               jobExample,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(f, cmd, args))
			cmdutil.CheckErr(o.Validate())
			cmdutil.CheckErr(o.Run())
		},
	}

	o.PrintFlags.AddFlags(cmd)

	cmdutil.AddApplyAnnotationFlags(cmd)
	cmdutil.AddValidateFlags(cmd)
	cmdutil.AddDryRunFlag(cmd)
	cmd.Flags().StringVar(&o.Image, "image", o.Image, "Image name to run.")
	cmd.Flags().StringVar(&o.From, "from", o.From, "The name of the resource to create a Job from (only cronjob is supported).")
	cmdutil.AddFieldManagerFlagVar(cmd, &o.FieldManager, "kubectl-create")
	return cmd
}

// Complete completes all the required options
func (o *CreateJobOptions) Complete(f cmdutil.Factory, cmd *cobra.Command, args []string) error {
	name, err := NameFromCommandArgs(cmd, args)
	if err != nil {
		return err
	}
	o.Name = name
	if len(args) > 1 {
		o.Command = args[1:]
	}

	clientConfig, err := f.ToRESTConfig()
	if err != nil {
		return err
	}
	o.Client, err = batchv1client.NewForConfig(clientConfig)
	if err != nil {
		return err
	}

	o.CreateAnnotation = cmdutil.GetFlagBool(cmd, cmdutil.ApplyAnnotationsFlag)

	o.Namespace, o.EnforceNamespace, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}
	o.Builder = f.NewBuilder()

	o.DryRunStrategy, err = cmdutil.GetDryRunStrategy(cmd)
	if err != nil {
		return err
	}
	cmdutil.PrintFlagsWithDryRunStrategy(o.PrintFlags, o.DryRunStrategy)
	printer, err := o.PrintFlags.ToPrinter()
	if err != nil {
		return err
	}
	o.PrintObj = func(obj runtime.Object) error {
		return printer.PrintObj(obj, o.Out)
	}

	o.ValidationDirective, err = cmdutil.GetValidationDirective(cmd)
	if err != nil {
		return err
	}

	return nil
}

// Validate makes sure provided values and valid Job options
func (o *CreateJobOptions) Validate() error {
	if (len(o.Image) == 0 && len(o.From) == 0) || (len(o.Image) != 0 && len(o.From) != 0) {
		return fmt.Errorf("either --image or --from must be specified")
	}
	if len(o.Command) != 0 && len(o.From) != 0 {
		return fmt.Errorf("cannot specify --from and command")
	}
	return nil
}

// Run performs the execution of 'create job' sub command
func (o *CreateJobOptions) Run() error {
	var job *batchv1.Job
	if len(o.Image) > 0 {
		job = o.createJob()
	} else {
		infos, err := o.Builder.
			WithScheme(scheme.Scheme, scheme.Scheme.PrioritizedVersionsAllGroups()...).
			NamespaceParam(o.Namespace).DefaultNamespace().
			ResourceTypeOrNameArgs(false, o.From).
			Flatten().
			Latest().
			Do().
			Infos()
		if err != nil {
			return err
		}
		if len(infos) != 1 {
			return fmt.Errorf("from must be an existing cronjob")
		}

		switch obj := infos[0].Object.(type) {
		case *batchv1.CronJob:
			job = o.createJobFromCronJob(obj)
		default:
			return fmt.Errorf("unknown object type %T", obj)
		}
	}

	if err := util.CreateOrUpdateAnnotation(o.CreateAnnotation, job, scheme.DefaultJSONEncoder()); err != nil {
		return err
	}

	if o.DryRunStrategy != cmdutil.DryRunClient {
		createOptions := metav1.CreateOptions{}
		if o.FieldManager != "" {
			createOptions.FieldManager = o.FieldManager
		}
		createOptions.FieldValidation = o.ValidationDirective
		if o.DryRunStrategy == cmdutil.DryRunServer {
			createOptions.DryRun = []string{metav1.DryRunAll}
		}
		var err error
		job, err = o.Client.Jobs(o.Namespace).Create(context.TODO(), job, createOptions)
		if err != nil {
			return fmt.Errorf("failed to create job: %v", err)
		}
	}

	return o.PrintObj(job)
}

func (o *CreateJobOptions) createJob() *batchv1.Job {
	job := &batchv1.Job{
		// this is ok because we know exactly how we want to be serialized
		TypeMeta: metav1.TypeMeta{APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name: o.Name,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    o.Name,
							Image:   o.Image,
							Command: o.Command,
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}
	if o.EnforceNamespace {
		job.Namespace = o.Namespace
	}
	return job
}

func (o *CreateJobOptions) createJobFromCronJob(cronJob *batchv1.CronJob) *batchv1.Job {
	annotations := make(map[string]string)
	annotations["cronjob.kubernetes.io/instantiate"] = "manual"
	for k, v := range cronJob.Spec.JobTemplate.Annotations {
		annotations[k] = v
	}

	job := &batchv1.Job{
		// this is ok because we know exactly how we want to be serialized
		TypeMeta: metav1.TypeMeta{APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        o.Name,
			Annotations: annotations,
			Labels:      cronJob.Spec.JobTemplate.Labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					// we are not using metav1.NewControllerRef because it
					// sets BlockOwnerDeletion to true which additionally mandates
					// cronjobs/finalizer role and not backwards-compatible.
					APIVersion: batchv1.SchemeGroupVersion.String(),
					Kind:       "CronJob",
					Name:       cronJob.GetName(),
					UID:        cronJob.GetUID(),
					Controller: ptr.To(true),
				},
			},
		},
		Spec: cronJob.Spec.JobTemplate.Spec,
	}
	if o.EnforceNamespace {
		job.Namespace = o.Namespace
	}
	return job
}
