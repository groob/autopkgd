Check autopkg recipes continuously(at a specified interval) and send notifications to a slack channel.
See config.toml.sample for a sample configuration.

autopkgd executes autopkg concurrently(separate process for each recipe in the recipe file). Because of this, autopkg must save each report plist in a separate file. You can specify a reports folder in the autopkgd config file.


