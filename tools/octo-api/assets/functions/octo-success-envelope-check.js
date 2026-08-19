module.exports = (schema, opts, context) => {
  const path = context && context.path || [];
  const operation = context && context.document && context.document.data;
  let operationId = '';
  if (operation && path.length >= 3) {
    const pathName = path[1];
    const method = path[2];
    operationId = operation.paths && operation.paths[pathName] && operation.paths[pathName][method] && operation.paths[pathName][method].operationId;
  }
  if (operationId === 'plugin.attachment.download' || operationId === 'plugin.archive.download') return;
  if (!schema || !schema.properties || !schema.properties.data) {
    return [{message: '2xx response must wrap payload in envelope (top-level `data` property required) (R1)'}];
  }
};
