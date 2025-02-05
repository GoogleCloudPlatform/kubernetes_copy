import os
import re
import datetime
import json


def extract_substrings(text, start_indices, end_indices):
    """
    Extract substrings from text based on start and end indices.
    Removes specific tags before returning the substrings.
    """
    if len(start_indices) != len(end_indices):
        raise ValueError("Start and end index lists must have the same length.")
    
    substrings = [
        text[start:end]
        .replace("<testdoc:step>", "")
        .replace("<testdoc:podspec>", "")
        .replace("<testdoc:name>", "")
        .replace("<testdoc:log>", "")
        for start, end in zip(start_indices, end_indices)
    ]
    
    return substrings


def find_strings_between(text, start, end):
    """
    Find all substrings between start and end patterns in the given text.
    """
    start_indices = [i for i in range(len(text)) if text.startswith(start, i)]
    end_indices = [i for i in range(len(text)) if text.startswith(end, i)]

    if not start_indices or not end_indices:
        return None

    return extract_substrings(text, start_indices, end_indices)


def find_test_key(mapper_value):
    """
    Find a key in 'mapper.json' corresponding to a given value.
    """
    try:
        with open('mapper.json', 'r') as file:
            mapper = json.load(file)

        return next((key for key, value in mapper.items() if value == mapper_value), None)
    except Exception as e:
        print(f"Error loading or parsing mapper.json: {e}")
        return None


def read_and_replace_content(file_path, name, steps,logs):
    """
    Read markdown file, find specific tags, replace them with given values, and save the updated content.
    """
    try:
        with open(file_path[0], 'r') as file:
            content = file.read()

        test_name = find_test_key(name[0].rsplit(":",1)[1])

        if not test_name:
            raise ValueError(f"Test name '{name[0]}' not found in mapper.")

        tag = f"<!-- {test_name} -->"

        # Replace the tag with steps
        updated_content = content.replace(tag, tag + "\n" + "\n".join(str(step) for step in steps)+"\n\n###Logs for the test\n" +"\n".join(str(log) for log in logs))

        print(f"Updated content:\n{updated_content}")

        # Save the modified content to a new file with a timestamped filename
        # output_file_name = f"file_{datetime.datetime.now().strftime('%Y-%m-%d_%H-%M-%S')}.txt"
        output_file_name = file_path[0]
        with open(output_file_name, 'w') as output_file:
            output_file.write(updated_content)

        print(f"File '{output_file_name}' has been successfully created.")

    except FileNotFoundError:
        print(f"File '{file_path}' not found.")
    except ValueError as e:
        print(e)
    except Exception as e:
        print(f"An error occurred: {e}")


# Example usage
if __name__ == "__main__":
    try:
        with open("build-log.txt", "r") as file:
            log_content = file.read()
        # Extract name and steps from the log content
        name = find_strings_between(log_content, "<testdoc:name>", "</testdoc:name>")
        namePath = ["content/en/docs/pod-lifecycle-events" + s.rsplit(':', 1)[0].replace(':','/') + ".md" for s in name]
        steps = find_strings_between(log_content, ("<testdoc:podspec>","<testdoc:step>"),("</testdoc:podspec>","</testdoc:step>"))
        logs = find_strings_between(log_content, "<testdoc:log>", "</testdoc:log>")
  
        if not name or not steps:
            print("Could not find the required tags in the log file.")
        else:
            # Process markdown file and replace content
            read_and_replace_content(namePath, name, steps,logs)

    except FileNotFoundError:
        print("Log file 'build-log.txt' not found.")
    except Exception as e:
        print(f"An error occurred: {e}")
