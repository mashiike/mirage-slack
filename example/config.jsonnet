local ssm = std.native('ssm');
local env = std.native('env');

{
  slack: {
    signing_secret: ssm('/mirage-slack/signing_secret'),
    bot_token: env('SLACK_BOT_TOKEN'),
    // list_name is optional (default: 'mirage-slack'). Uncomment to override:
    // list_name: 'mirage-slack-dev',
  },
  command: {
    name: '/mirage-slack',
  },
  routing: {
    // Optional fallback when no launched entry matches the incoming channel.
    // default_endpoint: 'https://main.example.com/slack',
    // default_endpoint_protect: true,  // default true
  },
}
