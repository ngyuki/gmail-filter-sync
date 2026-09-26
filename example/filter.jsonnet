local recipients = ['announce@example.com', 'no-reply@example.com'];
{
  filters: [{
    criteria: {
      query: 'from:{%s}' % std.join(' ', recipients),
    },
    action: { addLabels: ['STARRED'] },
  }],
}
