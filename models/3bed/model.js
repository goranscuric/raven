/* ~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~
 * spine & leaf system
 * ~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~*/

Switch = (name, level, mounts) => ({
  'name': name,
  'image': 'cumulus-latest',
  'os': 'linux',
  'level': level,
  'mounts': mounts
});

Node = (name, level, mounts, image, os) => ({
  'name': name,
  'image': image,
  'os': os,
  'level': level,
  'mounts': mounts
});

deter_mount = {
  'source': '/home/ry/deter',
  'point': '/opt/deter'
};

infra = ['boss', 'users', 'router'];
nodes = [
  ...Range(3).map(i => Node(`n${i}`, 3, [], 'debian-stretch', 'linux')),
  ...infra.map(n => Node(n, 1, [deter_mount], 'freebsd-11', 'freebsd')),
  Node('walrus', 
    2, [{
      'source': '/home/ry/deter/walrustf',
      'point': '/opt/walrus'
    }],
    'debian-stretch', 'linux'
  )
];

switches = [
  Switch('stem', 2, [deter_mount]),
  Switch('leaf', 4, [deter_mount])
];

links = [
  ...Range(3).map(i => Link(`${infra[i]}`, 'eth0', 'stem', `swp${i+1}`)),
  ...Range(3).map(i => Link(`n${i}`, 'eth0', 'stem', `swp${i+4}`)),
  ...Range(3).map(i => Link(`n${i}`, 'eth0', 'leaf', `swp${i+1}`)),
  Link('walrus', 'eth0', 'stem', 'swp7'),
  Link('stem', 'swp8', 'leaf', 'swp4')
];

topo = {
  'name': '3bed',
  'nodes': nodes,
  'switches': switches,
  'links': links
};


