# Copyright (c) 2012 The Chromium Authors. All rights reserved.
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

"""Set of utilities to add commands to a buildbot factory.

This is based on commands.py and adds skia-specific commands."""

from buildbot.process.properties import WithProperties
from master.factory import commands
from master.log_parser import retcode_command


class SkiaBuildStep(retcode_command.ReturnCodeCommand):
  """ BuildStep wrapper for Skia. Allows us to define properties of BuildSteps
  to be used by ShouldDoStep. This is necessary because the properties referred
  to by BuildStep.getProperty() are scoped for the entire duration of the build.
  """
  def __init__(self, is_upload_step=False, is_rebaseline_step=False, **kwargs):
    self._is_upload_step = is_upload_step
    self._is_rebaseline_step = is_rebaseline_step
    return retcode_command.ReturnCodeCommand.__init__(self, **kwargs)

  def IsUploadStep(self):
    return self._is_upload_step

  def IsRebaselineStep(self):
    return self._is_rebaseline_step


def _HasProperty(step, property):
  """ Helper used by ShouldDoStep. Determine whether the given BuildStep has
  the requested property.

  step: an instance of BuildStep
  property: string, the property to test
  """
  try:
    step.getProperty(property)
    return True
  except:
    return False


def ShouldDoStep(step):
  """ At build time, use build properties to determine whether or not a step
  should be run or skipped.

  step: an instance of BuildStep which we may or may not run.
  """
  print step.build.getProperties()
  if not isinstance(step, SkiaBuildStep):
    return True

  # If this step uploads results (and thus overwrites the most recently uploaded
  # results), only run it on scheduled builds (i.e. most recent revision) or if
  # the "force_upload" property was set.
  if step.IsUploadStep() and \
      not _HasProperty(step, 'scheduler') and \
      not _HasProperty(step, 'force_upload'):
    return False

  # When a commit consists of only new baseline images, we only need to run the
  # BuildSteps necessary for image verification, and only for the platform(s)
  # for which new baselines are provided.
  if  _HasProperty(step, 'branch') and \
      step.getProperty('branch') == 'gm-expected':
    if step.IsRebaselineStep():
      # This step is required for rebaselines, but do the associated commits
      # affect our platform?
      if _HasProperty(step, 'gm_image_subdir'):
        for change in step.build.allChanges():
          for file in change.asDict()['files']:
            if step.getProperty('gm_image_subdir') in file['name']:
              return True
    return False

  # Unless we have determined otherwise, run the step.
  return True


class SkiaCommands(commands.FactoryCommands):

  def __init__(self, factory, configuration, workdir, target_arch,
               default_timeout, target_platform, environment_variables):
    """Instantiates subclass of FactoryCommands appropriate for Skia.

    factory: a BaseFactory
    configuration: 'Debug' or 'Release'
    workdir: string indicating path within slave directory
    target_arch: string such as 'x64'
    default_timeout: default timeout for each command, in seconds
    target_platform: a string such as skia_factory.TARGET_PLATFORM_LINUX,
        to be passed into FactoryCommands.__init__()
    environment_variables: dictionary of environment variables that should
        be passed to all commands
    """
    commands.FactoryCommands.__init__(
        self, factory=factory, target=configuration,
        build_dir='', target_platform=target_platform)
    # Store some parameters that the subclass may want to use later.
    self.default_timeout = default_timeout
    self.environment_variables = environment_variables
    self.factory = factory
    self.target_arch = target_arch
    self.workdir = workdir
    # TODO(epoger): It would be better for this path to be specified by
    # an environment variable or some such, so that it is not dependent on the
    # path from CWD to slave/skia_slave_scripts... but for now, this will do.
    self._local_slave_script_dir = self.PathJoin(
        '..', '..', '..', '..', '..', '..', 'slave', 'skia_slave_scripts')

  def AddMergeIntoSvn(self, source_dir_path, dest_svn_url, merge_dir_path,
                      svn_username_file, svn_password_file,
                      commit_message=None, description='MergeIntoSvn',
                      timeout=None, is_rebaseline_step=False):
    """Adds a step that commits all files within a directory to a special SVN
    repository."""
    if not commit_message:
      commit_message = 'automated svn commit of %s step' % description
    args = ['--commit_message', commit_message,
            '--dest_svn_url', dest_svn_url,
            '--merge_dir_path', merge_dir_path,
            '--source_dir_path', source_dir_path,
            '--svn_password_file', svn_password_file,
            '--svn_username_file', svn_username_file,
            ]
    self.AddSlaveScript(script=self.PathJoin('utils', 'merge_into_svn.py'),
                        args=args, description=description, timeout=timeout,
                        is_upload_step=True,
                        is_rebaseline_step=is_rebaseline_step)

  def AddSlaveScript(self, script, args, description, timeout=None,
                     halt_on_failure=False, is_upload_step=False,
                     is_rebaseline_step=False):
    """Run a slave-side Python script as its own build step."""
    path_to_script = self.PathJoin(self._local_slave_script_dir, script)
    self.AddRunCommand(command=['python', path_to_script] + args,
                       description=description, timeout=timeout,
                       halt_on_failure=halt_on_failure,
                       is_upload_step=is_upload_step,
                       is_rebaseline_step=is_rebaseline_step)

  def AddRunCommand(self, command, description='Run', timeout=None,
                    halt_on_failure=False, is_upload_step=False,
                    is_rebaseline_step=False):
    """Runs an arbitrary command, perhaps a binary we built."""
    if not timeout:
      timeout = self.default_timeout
    self.factory.addStep(SkiaBuildStep,
                         is_upload_step=is_upload_step,
                         is_rebaseline_step=is_rebaseline_step,
                         description=description, timeout=timeout,
                         command=command, workdir=self.workdir,
                         env=self.environment_variables,
                         haltOnFailure=halt_on_failure,
                         doStepIf=ShouldDoStep)

  def AddRunCommandList(self, command_list, description='Run', timeout=None,
                        halt_on_failure=False, is_upload_step=False,
                        is_rebaseline_step=False):
    """Runs a list of arbitrary commands."""
    # TODO(epoger): Change this so that build-step output shows each command
    # in the list separately--that will be a lot easier to follow.
    #
    # TODO(epoger): For now, this wraps the total command with WithProperties()
    # because *some* callers need it, and we can't use the string.join() command
    # to concatenate strings that have already been wrapped with
    # WithProperties().  Once I figure out how to make the build-step output
    # show each command separately, maybe I can remove this wrapper.
    self.AddRunCommand(command=WithProperties(' && '.join(command_list)),
                       description=description, timeout=timeout,
                       halt_on_failure=halt_on_failure,
                       is_upload_step=is_upload_step,
                       is_rebaseline_step=is_rebaseline_step)
