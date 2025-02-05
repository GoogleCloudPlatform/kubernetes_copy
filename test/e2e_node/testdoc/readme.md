## Overview

This script automates the process of updating markdown files with test documentation extracted from log output file. It identifies specific `Area` and `Behavior` markers in the log content, then navigates the directory structure to locate markdown files with placeholders for these behaviors. The script inserts test details (steps, pod specifications, and logs) into the markdown files at the designated placeholders, ensuring each test case is documented properly.

This tool is particularly useful for maintaining up-to-date documentation for lifecycle events or tests in structured documentation projects.

## Functionality

- Structured Content Extraction: Parses the log content using custom tags (`<testdoc:name>`, `<testdoc:step>`, `<testdoc:podspec>`, and `<testdoc:log>`) to gather relevant test details.
- Dynamic Directory Navigation: Determines the target directory based on the `Area` specified in the `<testdoc:name>` tag, and identifies the correct placeholder in markdown files based on `Behavior`.
- Recursive File Processing: Iterates through the specified directory structure to find and update markdown files with matching placeholders.
- Content Ordering: Ensures that `<testdoc:podspec>` and `<testdoc:step>` entries appear in the order found in the log file, followed by the `<testdoc:log>` content.

## Usage

Run the following command:

```bash
python script_name.py path/to/log_file path/to/root_directory_documentation
```

- Replace `script_name.py` with the actual name of the Python script file.
- Replace `path/to/log_file` with the path to the log file (e.g., `log-output.txt`).
- Replace `path/to/root_directory` with the root directory containing the markdown files to be updated.

## Log File Format

The log file should include structured content for each test case, as shown below:

```plaintext
<testdoc:name>Area:Behavior</testdoc:name>
<testdoc:podspec>Details about the pod specification</testdoc:podspec>
<testdoc:step>Step details for the test</testdoc:step>
<testdoc:log>Log details</testdoc:log>
```

## Markdown Files Format

Each markdown file should include placeholders that correspond to `Behavior` values, using the following format:

```markdown
<!-- Behavior start -->
<!-- Behavior end -->
```

For example, if `Behavior` is `prestophook`, the placeholders should look like this:

```markdown
<!-- prestophook_basic start -->
<!-- prestophook_basic end -->
```

The script will locate these markers and insert the extracted content between them.

## Example

Given a log file (`log-output.txt`) with the following content:

```plaintext
<testdoc:name>lifecyclehooks:prestophook</testdoc:name>
<testdoc:podspec>Example pod specification...</testdoc:podspec>
<testdoc:step>Step 1: Description...</testdoc:step>
<testdoc:log>Log output details...</testdoc:log>
```

And a markdown file containing:

```markdown
# Lifecycle Hooks Documentation

<!-- prestophook start -->
<!-- prestophook end -->
```

Running the script as follows:

```bash
python script_name.py log-output.txt /path/to/documentation
```

will update the markdown file to:

```markdown
# Lifecycle Hooks Documentation

<!-- prestophook start -->
Example pod specification
Step 1: Description

### Logs for the test
Log output details
<!-- prestophook end -->
```

