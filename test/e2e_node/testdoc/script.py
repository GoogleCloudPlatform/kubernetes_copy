# Copyright 2024 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import os
import logging
import argparse

logging.basicConfig(level=logging.INFO, format="%(asctime)s - %(levelname)s - %(message)s")
logger = logging.getLogger(__name__)


def extract_substrings(text, start_indices, end_indices, remove_tags=True):
    if len(start_indices) != len(end_indices):
        raise ValueError("Start and end index lists must have the same length.")

    substrings = [text[start:end] for start, end in zip(start_indices, end_indices)]
    if remove_tags:
        substrings = [
            s.replace("<testdoc:step>", "")
             .replace("<testdoc:podspec>", "")
             .replace("<testdoc:name>", "")
             .replace("<testdoc:log>", "")
            for s in substrings
        ]
    return substrings


def find_strings_between(text, start, end):
    start_indices = [i for i in range(len(text)) if text.startswith(start, i)]
    end_indices = [i for i in range(len(text)) if text.startswith(end, i)]

    if not start_indices or not end_indices:
        return None, None

    return extract_substrings(text, start_indices, end_indices), start_indices


def read_and_replace_doc_content(file_path, behavior, steps, logs):
    try:
        with open(file_path, 'r+') as file:
            content = file.read()

            # Define the placeholder for this behavior
            tag_start = f"<!-- {behavior} start -->"
            tag_end = f"<!-- {behavior} end -->"

            # Check if both start and end tags exist
            start_index = content.find(tag_start)
            end_index = content.find(tag_end, start_index)

            if start_index != -1 and end_index != -1:
                # Prepare content to insert
                replacement_content = "\n".join(steps) + "\n\n### Logs for the test\n" + "\n".join(logs)
                updated_content = content[:start_index + len(tag_start)] + "\n" + replacement_content + "\n" + content[end_index:]

                # Write updated content back to the file
                file.seek(0)
                file.write(updated_content)
                file.truncate()
                logger.info(f"File '{file_path}' has been successfully updated for behavior '{behavior}'.")
            else:
                logger.warning(f"Placeholders for behavior '{behavior}' not found in '{file_path}'.")

    except FileNotFoundError:
        logger.warning(f"File '{file_path}' not found.")
    except ValueError as e:
        logger.error(f"Value error: {e}")
    except Exception as e:
        logger.error(f"An error occurred: {e}")


def process_directory(root_directory, area, behavior, steps, logs):
    target_dir = os.path.join(root_directory, area)
    if not os.path.isdir(target_dir):
        logger.warning(f"Directory '{target_dir}' does not exist.")
        return

    for root, _, files in os.walk(target_dir):
        for file in files:
            if file.endswith(".md"):
                file_path = os.path.join(root, file)
                logger.info(f"Processing file: {file_path}")
                read_and_replace_doc_content(file_path, behavior, steps, logs)


def main(log_file_path, root_directory):
    try:
        with open(log_file_path, "r") as file:
            log_content = file.read()

        # Extract test names and steps from the log content
        names, start_indices = find_strings_between(log_content, "<testdoc:name>", "</testdoc:name>")
        if not names:
            logger.error("No <testdoc:name> tags found in the log file.")
            return

        for index, name in enumerate(names):
            # Split name into area and behavior
            if ":" not in name:
                logger.warning(f"Invalid format for name '{name}'. Expected format 'Area:Behavior'. Skipping.")
                continue

            area, behavior = name.split(":", 1)
            steps = find_strings_between(log_content[start_indices[index]:], "<testdoc:step>", "</testdoc:step>")[0] or []
            podspecs = find_strings_between(log_content[start_indices[index]:], "<testdoc:podspec>", "</testdoc:podspec>")[0] or []
            logs = find_strings_between(log_content[start_indices[index]:], "<testdoc:log>", "</testdoc:log>")[0] or []

            # Combine podspecs and steps in the order they appear
            combined_steps = podspecs + steps

            # Process and update markdown files in the target directory for this area and behavior
            process_directory(root_directory, area, behavior, combined_steps, logs)

    except FileNotFoundError:
        logger.error(f"Log file '{log_file_path}' not found.")
    except Exception as e:
        logger.error(f"An error occurred: {e}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Update markdown files with extracted log content.")
    parser.add_argument("log_file_path", type=str, help="Path to the structured log file.")
    parser.add_argument("root_directory", type=str, help="Root directory containing markdown documentation.")
    args = parser.parse_args()

    main(args.log_file_path, args.root_directory)
