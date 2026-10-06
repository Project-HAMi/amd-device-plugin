# Configuration file for the Sphinx documentation builder.
#
# This file only contains a selection of the most common options. For a full
# list see the documentation:
# https://www.sphinx-doc.org/en/master/usage/configuration.html


external_projects_local_file = "projects.yaml"
external_projects_remote_repository = ""
#external_projects = ["k8s-device-plugin"]
external_projects = []
external_projects_current_project = "k8s-device-plugin"

project = "HAMi AMD Device Plugin Documentation"
version = "0.0.2"
release = version
html_title = f"Device Plugin Documentation {version}"
author = "The HAMi Authors"
copyright = "The HAMi Authors"

# Required settings
html_theme = "rocm_docs_theme"
html_theme_options = {
    "flavor": "instinct"
}
external_toc_path = "./sphinx/_toc.yml"

extensions = ["rocm_docs"]

exclude_patterns = ['.venv']
